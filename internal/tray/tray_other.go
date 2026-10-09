//go:build !darwin || !cgo

package tray

import (
	"context"
	"fmt"
)

// Available is false when the menu-bar tray cannot run (non-macOS or no cgo).
const Available = false

// Run reports that the tray is not available here.
func (t *Tray) Run(ctx context.Context) error {
	return fmt.Errorf("trebi tray is only available on macOS with cgo")
}
