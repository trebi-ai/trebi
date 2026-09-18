//go:build !ui

// Package ui serves the SPA embedded in a `-tags ui` build. In a plain build
// there is no UI and no static handler.
package ui

import "io/fs"

// FS reports that no SPA is embedded in a plain build.
func FS() (fs.FS, bool) { return nil, false }
