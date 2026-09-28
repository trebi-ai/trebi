// Package secrets owns ~/.trebi/secrets (0600 write-only values).
package secrets

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trebi-ai/trebi/internal/home"
)

// Secret redacts in logs.
type Secret string

// LogValue implements slog.LogValuer — never prints the value.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue("[secret]")
}

// A secret name is one file name: no separator, and no leading dot. The "@"
// is for the secrets of a workspace connector instance (name@ws_NAME).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]{0,127}$`)

// ErrBadName is a secret name that cannot be a plain file name.
var ErrBadName = errors.New("secret name must match " + nameRE.String())

// CheckName refuses a name that could reach outside the secrets directory.
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}

// Set writes a secret file 0600.
func Set(name, value string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	path := filepath.Join(home.SecretsDir(), name)
	return os.WriteFile(path, []byte(value), 0o600)
}

// Delete removes a secret.
func Delete(name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	path := filepath.Join(home.SecretsDir(), name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List names only.
func List() ([]string, error) {
	entries, err := os.ReadDir(home.SecretsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Get reads a secret value (for internal resolution only).
func Get(name string) (Secret, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(home.SecretsDir(), name))
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	return Secret(b), nil
}

// ExpandRefs replaces env values that match a secret name with the secret
// value. Names that are missing stay as the literal and are returned in missing.
func ExpandRefs(env map[string]string) (out map[string]string, missing []string) {
	if len(env) == 0 {
		return env, nil
	}
	names, err := List()
	if err != nil || len(names) == 0 {
		return env, nil
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	out = make(map[string]string, len(env))
	for k, v := range env {
		name := v
		if strings.HasPrefix(v, "secret:") {
			name = strings.TrimPrefix(v, "secret:")
		}
		if !have[name] {
			if strings.HasPrefix(v, "secret:") {
				missing = append(missing, name)
			}
			out[k] = v
			continue
		}
		sec, err := Get(name)
		if err != nil {
			missing = append(missing, v)
			out[k] = v
			continue
		}
		out[k] = string(sec)
	}
	return out, missing
}
