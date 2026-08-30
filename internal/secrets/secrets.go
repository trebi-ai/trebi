// Package secrets owns ~/.trebi/secrets (0600 write-only values).
package secrets

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/home"
)

// Secret redacts in logs.
type Secret string

// LogValue implements slog.LogValuer — never prints the value.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue("[secret]")
}

// Set writes a secret file 0600.
func Set(name, value string) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	path := filepath.Join(home.SecretsDir(), name)
	return os.WriteFile(path, []byte(value), 0o600)
}

// Delete removes a secret.
func Delete(name string) error {
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
		if !have[v] {
			out[k] = v
			continue
		}
		sec, err := Get(v)
		if err != nil {
			missing = append(missing, v)
			out[k] = v
			continue
		}
		out[k] = string(sec)
	}
	return out, missing
}
