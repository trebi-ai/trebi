// Package routingtest writes routes files for tests.
package routingtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/routing"
)

// Write writes <wsPath>/routes/<connection>.yaml from f and returns the path.
// A route with no id gets one, so a test can name it.
func Write(t testing.TB, wsPath, connection string, f routing.File) string {
	t.Helper()
	for i := range f.Routes {
		if f.Routes[i].ID == "" {
			f.Routes[i].ID = routing.NewID()
		}
	}
	data, err := routing.Marshal(f)
	if err != nil {
		t.Fatalf("marshal routes %s: %v", connection, err)
	}
	return WriteRaw(t, wsPath, connection, string(data))
}

// WriteRaw writes the text of a routes file.
func WriteRaw(t testing.TB, wsPath, connection, text string) string {
	t.Helper()
	dir := filepath.Join(wsPath, routing.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, connection+".yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
