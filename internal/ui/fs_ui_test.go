//go:build ui

package ui

import (
	"io/fs"
	"testing"
)

func TestFSWithTag(t *testing.T) {
	fsys, ok := FS()
	if !ok {
		t.Fatal("a ui build must embed the dist fixture")
	}
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		t.Fatalf("embedded dist lacks index.html: %v", err)
	}
}
