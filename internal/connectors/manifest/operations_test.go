package manifest_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

var update = flag.Bool("update", false, "rewrite the golden validator messages")

// TestOperationFixtures validates each fixture. A valid-*.yaml must pass;
// any other file must fail with the text in its .golden file.
func TestOperationFixtures(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/operations/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures %v %v", files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			t.Parallel()
			err := load(t, f).Validate()
			if strings.HasPrefix(filepath.Base(f), "valid-") {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var ve *manifest.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			var lines []string
			for _, fe := range ve.Fields {
				msg := fe.Message
				if strings.HasPrefix(msg, "validating ") {
					msg = "schema violation" // the schema library reports one violation, not always the same
				}
				lines = append(lines, fe.Field+": "+msg)
			}
			slices.Sort(lines)
			got := strings.Join(lines, "\n") + "\n"
			golden := strings.TrimSuffix(f, ".yaml") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("errors:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestOperationArgv(t *testing.T) {
	t.Parallel()
	m := load(t, "testdata/operations/valid-postgres.yaml")
	op, ok := m.Operation("query")
	if !ok {
		t.Fatal("no query operation")
	}
	hostile := `select 1; drop table x; $(rm -rf /) "q" 'q'`
	got, err := op.Argv(map[string]any{"sql": hostile, "params": map[string]any{"days": "7", "a_b": 3.0}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"psql", "--csv", "--no-psqlrc", "-X", "-v", "ON_ERROR_STOP=1", "-c", hostile, "-v", "a_b=3", "-v", "days=7"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv %q\nwant %q", got, want)
	}
	cases := []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"missing", map[string]any{}, []string{"input.sql"}},
		{"unknown", map[string]any{"sql": "x", "db": "y"}, []string{"input.db"}},
		{"bad key", map[string]any{"sql": "x", "params": map[string]any{"A;B": "1"}}, []string{"input.params"}},
		{"nested", map[string]any{"sql": "x", "params": map[string]any{"a": []any{"1"}}}, []string{"input.params"}},
		{"wrong type", map[string]any{"sql": 3.0}, []string{"input.sql"}},
	}
	for _, tc := range cases {
		_, err := op.Argv(tc.in)
		var ie *manifest.InputError
		if !errors.As(err, &ie) {
			t.Fatalf("%s: want InputError, got %v", tc.name, err)
		}
		var fields []string
		for _, f := range ie.Fields {
			fields = append(fields, f.Field)
		}
		if !slices.Equal(fields, tc.want) {
			t.Fatalf("%s: fields %v, want %v", tc.name, fields, tc.want)
		}
	}
}

func TestOperationNormalize(t *testing.T) {
	t.Parallel()
	m := load(t, "testdata/operations/valid-mailbox.yaml")
	op, _ := m.Operation("search")
	got, err := op.Normalize(map[string]any{"query": "invoice", "limit": "20"})
	if err != nil || got["limit"] != int64(20) {
		t.Fatalf("normalize %v %v", got, err)
	}
	for _, bad := range []any{"0", 101.0, 2.5, "many"} {
		if _, err := op.Normalize(map[string]any{"query": "x", "limit": bad}); err == nil {
			t.Fatalf("limit %v must fail", bad)
		}
	}
	if _, err := op.Normalize(map[string]any{"query": strings.Repeat("a", 501)}); err == nil {
		t.Fatal("a long query must fail")
	}
	in, err := op.StdinBytes(map[string]any{"query": "a", "limit": 2.0})
	if err != nil || string(in) != `{"limit":2,"query":"a"}` {
		t.Fatalf("stdin %s %v", in, err)
	}
	if !slices.Equal(m.OperationBinaries(), []string{"mailbox"}) {
		t.Fatalf("binaries %v", m.OperationBinaries())
	}
}

func TestOperationDefaultState(t *testing.T) {
	t.Parallel()
	m := load(t, "testdata/operations/valid-mailbox.yaml")
	want := map[string]manifest.OpState{"search": manifest.OpOn, "draft": manifest.OpOn, "send": manifest.OpOff}
	for name, st := range want {
		op, _ := m.Operation(name)
		if got := op.DefaultState(); got != st {
			t.Fatalf("%s default %s, want %s", name, got, st)
		}
	}
	if got := (manifest.Operation{ReadOnly: true}).DefaultState(); got != manifest.OpOn {
		t.Fatalf("read-only default %s", got)
	}
	if got := (manifest.Operation{}).EffectiveTimeout(); got != manifest.DefaultOpTimeout {
		t.Fatalf("timeout %s", got)
	}
}
