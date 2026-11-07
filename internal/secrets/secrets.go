// Package secrets owns ~/.trebi/secrets (0600 write-only values).
package secrets

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

// ErrReserved is a user write to a name that Trebi owns.
var ErrReserved = errors.New("secret name is reserved for Trebi")

// ReservedPrefix starts the names that Trebi owns: the OAuth logins of
// connections. List hides them, and a user cannot write them.
const ReservedPrefix = "oauth."

// CheckName refuses a name that could reach outside the secrets directory.
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}

// Reserved reports whether Trebi owns a secret name.
func Reserved(name string) bool { return strings.HasPrefix(name, ReservedPrefix) }

// CheckUserName refuses a name that CheckName refuses, or a reserved one.
func CheckUserName(name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if Reserved(name) {
		return fmt.Errorf("%w: %q", ErrReserved, name)
	}
	return nil
}

// Dir is one secrets directory. The package functions use the home one.
type Dir string

// HomeDir is the secrets directory of this home.
func HomeDir() Dir { return Dir(home.SecretsDir()) }

// Set writes a secret file 0600.
func (d Dir) Set(name, value string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(string(d), name), []byte(value), 0o600)
}

// Get reads a secret value.
func (d Dir) Get(name string) (Secret, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(string(d), name))
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	return Secret(b), nil
}

// Has reports whether the secret exists.
func (d Dir) Has(name string) bool {
	if CheckName(name) != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(string(d), name))
	return err == nil
}

// Delete removes a secret.
func (d Dir) Delete(name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(string(d), name)); err != nil && !os.IsNotExist(err) {
		return err
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
	return HomeDir().Set(name, value)
}

// Delete removes a secret.
func Delete(name string) error { return HomeDir().Delete(name) }

// List names only. It hides the reserved names.
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
		if !e.IsDir() && !Reserved(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Get reads a secret value (for internal resolution only).
func Get(name string) (Secret, error) { return HomeDir().Get(name) }

// ResolveRefs is ExpandRefs into a new map that drops each "secret:" value
// with no secret, so that a reference never passes as a value.
func ResolveRefs(env map[string]string) map[string]string {
	out, _ := ExpandRefs(env)
	out = maps.Clone(out)
	for k, v := range out {
		if v == env[k] && strings.HasPrefix(v, "secret:") {
			delete(out, k)
		}
	}
	return out
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
