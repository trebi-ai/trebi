package state

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseCollections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		err  string
	}{
		{"none", ``, ""},
		{"empty decl", "sources:\n", ""},
		{"full", `
reported:
  description: Items already sent.
  ttl: 2y
  schema:
    type: object
    required: [title]
    properties:
      title: {type: string}
`, ""},
		{"bad name", "Reported: {}\n", `collection "Reported": name must match`},
		{"bad ttl", "a: {ttl: soon}\n", `collection "a": ttl "soon"`},
		{"unknown field", "a: {size: 3}\n", `collection "a": fields are`},
		{"schema not object", "a: {schema: {type: array}}\n", "type must be object"},
		{"bad schema", "a: {schema: {type: object, properties: {x: {type: 7}}}}\n", `collection "a": schema`},
		{"not a map", "- a\n", "must be a map"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v any
			if err := yaml.Unmarshal([]byte(tc.yaml), &v); err != nil {
				t.Fatal(err)
			}
			cols, err := ParseCollections(v)
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.HasPrefix(err.Error(), "state: ") {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if cols != nil {
				t.Fatalf("cols = %v", cols)
			}
		})
	}
}
