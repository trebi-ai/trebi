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
func APIJSONPath() string    { return filepath.Join(Base(), "api.json") }
func DaemonLockPath() string { return filepath.Join(Base(), "daemon.lock") }

// RelayLoopbackPath holds the phone tokens of the relaytest loopback.
func RelayLoopbackPath() string { return filepath.Join(Base(), "relay-loopback.json") }

func ContextPath() string { return filepath.Join(Base(), "context") }
func LinkedWorkspacesPath() string {
	return filepath.Join(Base(), "linked-workspaces.yaml")
}
func SecretsDir() string { return filepath.Join(Base(), "secrets") }
func SkillsDir() string  { return filepath.Join(Base(), "skills") }
func HarnessDir(name string) string {
	return filepath.Join(Base(), "harness", name)
}
func BinDir() string { return filepath.Join(Base(), "bin") }

// ConnectorCommandsDir holds one link for each command of an installed
// connector. A user adds it to the PATH of the shell once.
func ConnectorCommandsDir() string { return filepath.Join(BinDir(), "connectors") }

// HarnessBackupDir holds the reset copies of the config files of a harness.
func HarnessBackupDir(name string) string {
	return filepath.Join(Base(), "backups", "harness", name)
}
func LibexecDir() string { return filepath.Join(Base(), "libexec") }
func LogsDir() string    { return filepath.Join(Base(), "logs") }
func RunsLogDir() string { return filepath.Join(Base(), "logs", "runs") }
func RunsDir() string    { return filepath.Join(Base(), "runs") }

// RunDir holds the input files of one run, such as the events.json of a
// batch run.
func RunDir(id string) string { return filepath.Join(RunsDir(), id) }

// RequestsDir holds the report snapshots of requests, one folder per id.
func RequestsDir() string { return filepath.Join(Base(), "requests") }

// ArtifactsDir holds the artifact store.
func ArtifactsDir() string { return filepath.Join(Base(), "artifacts") }

// ArtifactBlobsDir holds the artifact bytes, named by their sha256.
func ArtifactBlobsDir() string { return filepath.Join(ArtifactsDir(), "blobs", "sha256") }

// UploadsDir holds the images that a person adds to a message, one folder
// per workspace.
func UploadsDir() string { return filepath.Join(Base(), "uploads") }

// ConnectionsDir holds one folder per connection, named by its id.
func ConnectionsDir() string { return filepath.Join(Base(), "connectors") }

// ConnectionDir is the folder of one connection: the unit of backup and
// restore.
func ConnectionDir(id string) string { return filepath.Join(ConnectionsDir(), id) }

// PackagesDir holds the unpacked catalog packages, shared by type.
func PackagesDir() string { return filepath.Join(Base(), "packages", "connectors") }

// PackageDir is the unpacked snapshot of one connector type at one version.
func PackageDir(typ, version string) string { return filepath.Join(PackagesDir(), typ, version) }

// ConnectorStagingRoot holds the folders that a link check fetched.
func ConnectorStagingRoot() string { return filepath.Join(PackagesDir(), ".staging") }

// ConnectorStagingDir is the folder of one link check.
func ConnectorStagingDir(id string) string { return filepath.Join(ConnectorStagingRoot(), id) }

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
