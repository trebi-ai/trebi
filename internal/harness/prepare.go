// Package harness materializes jobi-owned config trees under ~/.trebi/harness
// and bridges auth from the user's interactive profile when mode is shared.
package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
)

// Harness schema version; bumped when defaults under harness/<tool> change.
const SchemaVersion = 1

// AuthModeShared bridges user credentials into the jobi harness tree.
const AuthModeShared = "shared"

// AuthModeSeparate keeps credentials only under jobi (no user symlink).
const AuthModeSeparate = "separate"

// EnsureDir creates dir with 0755 if missing.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0755)
}

// WriteFileIfChanged writes data at path with mode; skips when identical.
func WriteFileIfChanged(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SymlinkAuth creates dst → src if src exists and dst is missing or wrong.
// Does nothing when mode is separate or src is missing.
func SymlinkAuth(src, dst, mode string) error {
	if mode == AuthModeSeparate {
		// Remove shared bridge if present so separate login is not mixed.
		if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			_ = os.Remove(dst)
		}
		return nil
	}
	if src == "" || dst == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return nil // missing user auth — doctor/check will report
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(dst)
			if target == src {
				return nil
			}
			_ = os.Remove(dst)
		} else {
			// Real file already present (separate login or copy) — leave it.
			return nil
		}
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", dst, src, err)
	}
	return nil
}

// UserHome returns the user home directory or empty.
func UserHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// AuthModeFromConfig returns shared|separate for tool from loaded config.
func AuthModeFromConfig(cfg *config.Config, tool string) string {
	if cfg == nil {
		return AuthModeShared
	}
	return cfg.Tools.AuthMode(tool)
}

// LoadSecretEnv reads KEY=value lines from ~/.trebi/secrets/<tool>.env.
// Values are never logged by this function.
func LoadSecretEnv(tool string) (map[string]string, error) {
	path := config.SecretEnvPath(tool)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out, nil
}

// WriteSecretEnv writes KEY=value pairs with mode 0600 under secrets/.
func WriteSecretEnv(tool string, env map[string]string) error {
	if err := os.MkdirAll(config.SecretsDir(), 0700); err != nil {
		return err
	}
	var b strings.Builder
	for k, v := range env {
		if k == "" {
			continue
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	path := config.SecretEnvPath(tool)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveSecretEnv deletes the secrets env file if present.
func RemoveSecretEnv(tool string) error {
	err := os.Remove(config.SecretEnvPath(tool))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WriteSchemaStamp writes .trebi-harness-schema under harness dir.
func WriteSchemaStamp(harnessDir string) error {
	return WriteFileIfChanged(
		filepath.Join(harnessDir, ".trebi-harness-schema"),
		[]byte(fmt.Sprintf("%d\n", SchemaVersion)),
		0644,
	)
}
