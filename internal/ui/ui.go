//go:build ui

// Package ui serves the SPA embedded in a `-tags ui` build. The dist tree is
// written by trebi-ui's build:embed (or by the release workflow unpacking a
// trebi-ui release tarball) and is git-ignored.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS returns the embedded SPA rooted at dist. The boolean is false in a
// plain build.
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
