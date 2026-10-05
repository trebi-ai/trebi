// Package ext is the extension contract between the public trebi module and
// a wrapping edition. It imports nothing from this repository.
package ext

import "io/fs"

// Set is everything a wrapping main can inject. A nil field is the free
// behaviour. Add a field only when a second implementation exists.
type Set struct {
	Edition        string // "" → free; "pro" from trebi-pro
	EditionVersion string // trebi-pro short commit; "" on free
	UI             fs.FS  // embedded SPA; nil → no UI served
	UIVersion      string // version of UI; "" when UI is nil
}
