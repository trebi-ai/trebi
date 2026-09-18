//go:build !ui

package ui

import "testing"

func TestFSWithoutTag(t *testing.T) {
	if _, ok := FS(); ok {
		t.Fatal("a plain build must not embed a UI")
	}
}
