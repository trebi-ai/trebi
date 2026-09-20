package app_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The public surface is app and ext. Everything else stays under internal/
// or is a main package under cmd/.
func TestPublicPackages(t *testing.T) {
	t.Parallel()
	root := ".."
	var got []string
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		switch name {
		case "internal", "cmd", "scripts", "evals", "testdata":
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !dirHasGo(t, filepath.Join(root, name)) {
			continue
		}
		got = append(got, name)
	}
	slices.Sort(got)
	want := []string{"app", "ext"}
	if !slices.Equal(got, want) {
		t.Fatalf("public packages %v, want %v", got, want)
	}
}

func TestAppExportsOnlyMain(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "app.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var exported []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				exported = append(exported, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						exported = append(exported, s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							exported = append(exported, n.Name)
						}
					}
				}
			}
		}
	}
	if !slices.Equal(exported, []string{"Main"}) {
		t.Fatalf("exported %v, want [Main]", exported)
	}
}

func dirHasGo(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
