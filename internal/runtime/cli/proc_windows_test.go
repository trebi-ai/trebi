//go:build windows

package cli

import "testing"

// TestRejectShimRefusesCmd verifies the Windows shim refusal (Part D): npm
// .cmd/.bat wrappers re-parse the command line and cannot carry the wire.
func TestRejectShimRefusesCmd(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		`C:\Users\x\AppData\Roaming\npm\claude.cmd`,
		`C:\tools\codex.BAT`,
	} {
		if err := rejectShim(p); err == nil {
			t.Fatalf("shim accepted: %s", p)
		}
	}
	if err := rejectShim(`C:\Users\x\.local\bin\claude.exe`); err != nil {
		t.Fatalf("native exe refused: %v", err)
	}
}
