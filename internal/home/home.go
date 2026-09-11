// Package home owns ~/.trebi paths and TREBI_HOME override for tests.
package home

import (
	"os"
	"path/filepath"
	"sync"
)

//nolint:gochecknoglobals // process-wide home override; the exported function API needs shared state
var (
	mu   sync.RWMutex
	base string // empty → derive from env / user home
)

// Base returns the trebi home directory (default ~/.trebi).
func Base() string {
	mu.RLock()
	if base != "" {
		b := base
		mu.RUnlock()
		return b
	}
	mu.RUnlock()
	if env := os.Getenv("TREBI_HOME"); env != "" {
		return env
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".trebi"
	}
	return filepath.Join(h, ".trebi")
}

// SetBase overrides Base for tests. Pass "" to clear.
func SetBase(dir string) {
	mu.Lock()
	base = dir
	mu.Unlock()
}

// Paths under home.
func DBPath() string         { return filepath.Join(Base(), "trebi.db") }
func ConfigPath() string     { return filepath.Join(Base(), "config.yaml") }
func ConnectorsPath() string { return filepath.Join(Base(), "connectors.yaml") }
func APIJSONPath() string    { return filepath.Join(Base(), "api.json") }
func DaemonLockPath() string { return filepath.Join(Base(), "daemon.lock") }
func ContextPath() string    { return filepath.Join(Base(), "context") }
func LinkedWorkspacesPath() string {
	return filepath.Join(Base(), "linked-workspaces.yaml")
}
func SecretsDir() string { return filepath.Join(Base(), "secrets") }
func SkillsDir() string  { return filepath.Join(Base(), "skills") }
func HarnessDir(name string) string {
	return filepath.Join(Base(), "harness", name)
}
func BinDir() string     { return filepath.Join(Base(), "bin") }
func LibexecDir() string { return filepath.Join(Base(), "libexec") }
func LogsDir() string    { return filepath.Join(Base(), "logs") }
func RunsLogDir() string { return filepath.Join(Base(), "logs", "runs") }
func RunsDir() string    { return filepath.Join(Base(), "runs") }
func ConnectorsLogDir() string {
	return filepath.Join(Base(), "logs", "connectors")
}
func DaemonLogPath() string { return filepath.Join(Base(), "logs", "trebid.log") }

// EnsureDirs creates the standard directory tree.
func EnsureDirs() error {
	dirs := []string{
		Base(),
		SkillsDir(),
		filepath.Join(Base(), "harness"),
		BinDir(),
		LibexecDir(),
		LogsDir(),
		RunsLogDir(),
		RunsDir(),
		ConnectorsLogDir(),
		filepath.Join(Base(), "state"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return ensureSecretsDir()
}

// ensureSecretsDir creates the secrets dir at 0700 and fixes a wrong mode.
func ensureSecretsDir() error {
	d := SecretsDir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(d)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(d, 0o700)
	}
	return nil
}
