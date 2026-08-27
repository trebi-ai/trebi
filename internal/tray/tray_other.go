//go:build !darwin || !cgo

package tray

import (
	"context"
	"fmt"
)

// Available is false when the menu-bar tray cannot run (non-macOS or no cgo).
const Available = false

// Run is a no-op stub; callers should use --no-tray or run headless.
func Run(ctx context.Context, onQuit func()) error {
	return fmt.Errorf("trebi tray is only available on macOS with cgo")
}
