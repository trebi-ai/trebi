package folderenv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		vals    map[string]string
		keys    []string
		skipped []Skip
	}{
		{name: "plain", in: "A=1\nB = two ", vals: map[string]string{"A": "1", "B": "two"}, keys: []string{"A", "B"}},
		{name: "export", in: "export A=1\nexport\tB=2\nexporter=3", vals: map[string]string{"A": "1", "B": "2", "exporter": "3"}, keys: []string{"A", "B", "exporter"}},
		{name: "single quote literal", in: `A='x \n # y'`, vals: map[string]string{"A": `x \n # y`}, keys: []string{"A"}},
		{name: "double quote escapes", in: `A="l1\nl2 \"q\" \\ \t" # c`, vals: map[string]string{"A": "l1\nl2 \"q\" \\ \\t"}, keys: []string{"A"}},
		{name: "inline comment", in: "A=val # note\nB=#hash\nC= #empty\nD=a#b", vals: map[string]string{"A": "val", "B": "#hash", "C": "", "D": "a#b"}, keys: []string{"A", "B", "C", "D"}},
		{name: "comments and blanks", in: "# top\n\n   \n  # indented\nA=1", vals: map[string]string{"A": "1"}, keys: []string{"A"}},
		{name: "duplicate", in: "A=1\nB=2\nA=3", vals: map[string]string{"A": "3", "B": "2"}, keys: []string{"A", "B"}},
		{name: "crlf", in: "A=1\r\nB=\"2\"\r\n", vals: map[string]string{"A": "1", "B": "2"}, keys: []string{"A", "B"}},
		{name: "backtick literal", in: "A=`it's \"x\"`", vals: map[string]string{"A": `it's "x"`}, keys: []string{"A"}},
		{name: "quotes inside other quotes", in: `A="it's"` + "\nB='say \"hi\"'", vals: map[string]string{"A": "it's", "B": `say "hi"`}, keys: []string{"A", "B"}},
		{
			name: "multi-line double", in: "A=\"-----BEGIN KEY-----\nline 2 \\\"q\\\"\n  indented\n-----END KEY-----\" # c\nB=after",
			vals: map[string]string{"A": "-----BEGIN KEY-----\nline 2 \"q\"\n  indented\n-----END KEY-----", "B": "after"}, keys: []string{"A", "B"},
		},
		{
			name: "multi-line single and backtick", in: "A='one\n# not a comment\ntwo'\nB=`x\ny=z`\nC=3",
			vals: map[string]string{"A": "one\n# not a comment\ntwo", "B": "x\ny=z", "C": "3"}, keys: []string{"A", "B", "C"},
		},
		{name: "multi-line crlf", in: "A=\"l1\r\nl2\"\r\nB=2\r\n", vals: map[string]string{"A": "l1\nl2", "B": "2"}, keys: []string{"A", "B"}},
		{
			name: "unclosed quote skips one line", in: "A=1\nB=\"never\nC=3\nD='x",
			vals: map[string]string{"A": "1", "C": "3"}, keys: []string{"A", "C"},
			skipped: []Skip{{2, ReasonQuoteNotEnd}, {4, ReasonQuoteNotEnd}},
		},
		{name: "no expansion", in: "A=${HOME}/x", vals: map[string]string{"A": "${HOME}/x"}, keys: []string{"A"}},
		{
			name: "skips", in: "A=1\njunk\n1BAD=x\nmy-key=x\nD=4",
			vals: map[string]string{"A": "1", "D": "4"}, keys: []string{"A", "D"},
			skipped: []Skip{{2, ReasonNoEquals}, {3, ReasonBadKey}, {4, ReasonBadKey}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vals, keys, skipped := Parse([]byte(tc.in))
			if !reflect.DeepEqual(vals, tc.vals) {
				t.Errorf("vals = %q, want %q", vals, tc.vals)
			}
			if !reflect.DeepEqual(keys, tc.keys) {
				t.Errorf("keys = %q, want %q", keys, tc.keys)
			}
			if !reflect.DeepEqual(skipped, tc.skipped) {
				t.Errorf("skipped = %v, want %v", skipped, tc.skipped)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	if f := Load(""); f != nil {
		t.Fatalf("empty dir = %+v, want nil", f)
	}
	if f := Load(t.TempDir()); f != nil {
		t.Fatalf("missing file = %+v, want nil", f)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Name), []byte("TOKEN=secret-value\nbad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := Load(dir)
	if f == nil || f.Err != "" || f.Values["TOKEN"] != "secret-value" {
		t.Fatalf("load = %+v", f)
	}
	s := f.Summary(true)
	if !s.Found || !s.Enabled || s.Path != filepath.Join(dir, Name) || !reflect.DeepEqual(s.Keys, []string{"TOKEN"}) || len(s.Skipped) != 1 {
		t.Fatalf("summary = %+v", s)
	}

	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, Name), []byte("A="+strings.Repeat("x", MaxSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := Load(big); f == nil || f.Err == "" || f.Values != nil {
		t.Fatalf("large file = %+v, want an error and no values", f)
	}

	sub := t.TempDir()
	if err := os.Mkdir(filepath.Join(sub, Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if f := Load(sub); f == nil || f.Err != "not a regular file" {
		t.Fatalf("dir named .env = %+v", f)
	}

	if s := (*File)(nil).Summary(false); s.Found || s.Keys == nil {
		t.Fatalf("nil summary = %+v", s)
	}
}
