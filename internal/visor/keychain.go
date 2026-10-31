package visor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// errSecItemNotFound is the exit code of security for a missing item.
const errSecItemNotFound = 44

// claudeKeychain moves the macOS login item of Claude when its config folder
// moves. Claude names the item after the folder, so the old item is not
// found from the new folder.
type claudeKeychain struct {
	security string // the security program
}

// claudeService is the item name that Claude uses for a config folder.
func claudeService(dir string) string {
	sum := sha256.Sum256([]byte(norm.NFC.String(dir)))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

// move copies the item of oldDir to newDir and deletes the old item. A
// missing old item is not an error. The value never goes to a log or an
// error.
func (k claudeKeychain) move(ctx context.Context, oldDir, newDir string) error {
	oldSvc, newSvc := claudeService(oldDir), claudeService(newDir)
	for _, acct := range claudeAccounts() {
		out, err := exec.CommandContext(ctx, k.security, "find-generic-password", "-s", oldSvc, "-a", acct, "-w").Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == errSecItemNotFound {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", oldSvc, err)
		}
		value := strings.TrimSuffix(string(out), "\n")
		if err := exec.CommandContext(ctx, k.security, "add-generic-password", "-U", "-s", newSvc, "-a", acct, "-w", value).Run(); err != nil {
			return fmt.Errorf("write %s: %w", newSvc, err)
		}
		if err := exec.CommandContext(ctx, k.security, "delete-generic-password", "-s", oldSvc, "-a", acct).Run(); err != nil {
			return fmt.Errorf("delete %s: %w", oldSvc, err)
		}
		return nil
	}
	return nil
}

// claudeAccounts are the accounts that Claude can use for its item: $USER,
// else "claude-code-user". The visor service can run without $USER, so the
// user record comes second.
func claudeAccounts() []string {
	var name string
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	var accts []string
	for _, a := range []string{os.Getenv("USER"), name, "claude-code-user"} {
		if a != "" && !slices.Contains(accts, a) {
			accts = append(accts, a)
		}
	}
	return accts
}
