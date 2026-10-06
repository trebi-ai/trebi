package job_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

const testID = "jb_0123456789"

func TestWithID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "no frontmatter", in: "Do work.\n", want: "---\nid: " + testID + "\n---\nDo work.\n"},
		{name: "empty file", in: "", want: "---\nid: " + testID + "\n---\n"},
		{name: "frontmatter with no id", in: "---\nharness: claude\n---\n\nDo work.\n", want: "---\nid: " + testID + "\nharness: claude\n---\n\nDo work.\n"},
		{name: "old id not first", in: "---\nharness: claude\nid: jb_ffffffffff\ntitle: X\n---\nBody id: keep\n", want: "---\nid: " + testID + "\nharness: claude\ntitle: X\n---\nBody id: keep\n"},
		{name: "same id first", in: "---\nid: " + testID + "\nharness: claude\n---\n", want: "---\nid: " + testID + "\nharness: claude\n---\n"},
		{name: "nested id stays", in: "---\nharness: claude\nmeta:\n  id: inner\n---\n", want: "---\nid: " + testID + "\nharness: claude\nmeta:\n  id: inner\n---\n"},
		{name: "crlf", in: "---\r\nharness: claude\r\nid: old\r\n---\r\nDo work.\r\n", want: "---\r\nid: " + testID + "\r\nharness: claude\r\n---\r\nDo work.\r\n"},
		{name: "crlf no frontmatter", in: "Do work.\r\n", want: "---\r\nid: " + testID + "\r\n---\r\nDo work.\r\n"},
		{name: "unclosed block", in: "---\nharness: claude\n\nDo work.\n", wantErr: true},
		{name: "dashes only", in: "---", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := job.WithID([]byte(tc.in), testID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestWithIDParses(t *testing.T) {
	t.Parallel()
	out, err := job.WithID([]byte("---\nharness: claude\n---\n\nDo work.\n"), testID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := job.ParseBytes("/ws/a/JOB.md", out)
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != testID || j.IDState != job.IDOK {
		t.Fatalf("id=%q state=%v", j.ID, j.IDState)
	}
}

func TestSetIDKeepsOtherBytes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "JOB.md")
	in := "---\nharness: claude # main\ntriggers:\n  - every: 1h\n---\n\nDo work.\n\n---\nNot frontmatter.\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := job.SetID(path, testID); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "---\nid: " + testID + "\n" + in[len("---\n"):]; string(got) != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestValidID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind, id string
		want     bool
	}{
		{"job", "jb_0123456789", true},
		{"job", "jb_abcdef0123", true},
		{"agent", "agt_0123456789", true},
		{"job", "agt_0123456789", false},
		{"agent", "jb_0123456789", false},
		{"job", "jb_012345678", false},
		{"job", "jb_01234567890", false},
		{"job", "jb_ABCDEF0123", false},
		{"job", "jb_012345678g", false},
		{"job", "JB_0123456789", false},
		{"job", " jb_0123456789", false},
		{"job", "", false},
		{"job", "alpha", false},
	}
	for _, tc := range cases {
		if got := job.ValidID(tc.kind, tc.id); got != tc.want {
			t.Errorf("ValidID(%q, %q)=%t want %t", tc.kind, tc.id, got, tc.want)
		}
	}
	for _, kind := range []string{"job", "agent"} {
		id := job.NewID(kind)
		if !job.ValidID(kind, id) || !job.LooksLikeID(id) {
			t.Errorf("NewID(%q)=%q is not valid", kind, id)
		}
	}
	if a, b := job.NewID("job"), job.NewID("job"); a == b {
		t.Error("NewID repeats")
	}
}
