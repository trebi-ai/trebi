package ext_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The contract: ext imports nothing from this repository. A dependency on
// any internal package would make the seam unusable from a wrapping edition.
func TestExtImportsNothingFromRepository(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "github.com/trebi-ai/trebi") {
				t.Errorf("%s imports %s; ext must stay a leaf", entry.Name(), path)
			}
		}
	}
}
