// Package ext is the extension contract between the public trebi module and
// a wrapping edition. It imports nothing from this repository.
package ext

import (
	"errors"
	"io/fs"
	"time"
)

// Set is everything a wrapping main can inject. A nil field is the free
// behaviour. Add a field only when a second implementation exists.
type Set struct {
	Edition        string          // "" → free; "pro" from trebi-pro
	EditionVersion string          // trebi-pro release tag; "" on free
	UI             fs.FS           // embedded SPA; nil → no UI served
	License        LicenseVerifier // nil → no license support
}

// LicenseVerifier reads and checks an installed license.
type LicenseVerifier interface {
	// Verify parses raw, checks its signature and expiry, and returns the
	// entitlement. ErrInvalid on any failure.
	Verify(raw []byte, now time.Time) (Entitlement, error)
}

type Entitlement struct {
	Plan      string
	Features  []string
	ExpiresAt time.Time
	Subject   string // licensee display
}

var ErrInvalid = errors.New("license invalid")
