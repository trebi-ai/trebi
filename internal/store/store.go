// Package store owns SQLite open, migrations, and low-level helpers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	_ "modernc.org/sqlite"
)

// ArchivedJobIDsSQL selects the IDs of live jobs with an effective archive:
// the job has the flag, or its owner agent has it.
const ArchivedJobIDsSQL = `SELECT j.id FROM job_index j LEFT JOIN job_index o ON o.id = j.owner_id AND j.owner_id <> '' WHERE j.retired_at IS NULL AND (j.archived_at IS NOT NULL OR o.archived_at IS NOT NULL)`

// Store is a trebi database handle.
type Store struct {
	db     *sql.DB
	path   string
	mu     sync.Mutex // serializes bus appends (gapless seq)
	wakeMu sync.Mutex
	wake   chan struct{}
}

// Open opens (or creates) trebi.db under home and runs migrations.
func Open() (*Store, error) {
	if err := home.EnsureDirs(); err != nil {
		return nil, err
	}
	return OpenPath(home.DBPath())
}

// OpenPath opens a database at path (tests use temp paths).
func OpenPath(path string) (*Store, error) {
	if err := ensureParent(path); err != nil {
		return nil, err
	}
	// journal_size_limit cuts the WAL file back to 64 MiB after a checkpoint; a VACUUM or a large prune grows it.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=journal_size_limit(67108864)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(8)
	if err := migrate(d); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return &Store{db: d, path: path, wake: make(chan struct{})}, nil
}

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

// DB returns the underlying *sql.DB.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SchemaVersion returns the applied head version.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// WithTx runs fn inside a transaction.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WithBusTx runs fn in a transaction under the bus lock, then wakes the bus
// waiters after a successful commit. Lock order: bus lock, then the tx.
func (s *Store) WithBusTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.LockBus()
	err := s.WithTx(ctx, fn)
	s.UnlockBus()
	if err == nil {
		s.NotifyBus()
	}
	return err
}

// LockBus serializes event_log inserts so seq is gapless-in-commit-order.
func (s *Store) LockBus()   { s.mu.Lock() }
func (s *Store) UnlockBus() { s.mu.Unlock() }

// NotifyBus wakes SSE/alert waiters after a committed append.
func (s *Store) NotifyBus() {
	s.wakeMu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.wakeMu.Unlock()
}

// BusWake returns a channel that closes on the next bus append.
func (s *Store) BusWake() <-chan struct{} {
	s.wakeMu.Lock()
	ch := s.wake
	s.wakeMu.Unlock()
	return ch
}

// ParseTS parses RFC3339Nano then RFC3339. Always UTC. Zero on failure.
func ParseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t.UTC()
}

// ErrSchemaTooOld stops the daemon on a database from before baseVersion.
var ErrSchemaTooOld = errors.New("this Trebi database is from an older version and cannot be upgraded. Move ~/.trebi/trebi.db away and start Trebi again to make a new one")

func migrate(d *sql.DB) error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	var current int
	if err := d.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if current > 0 && current < baseVersion {
		return fmt.Errorf("%w (version %d)", ErrSchemaTooOld, current)
	}
	if current == 0 {
		if err := applyStep(d, baseVersion, baseSchema()); err != nil {
			return err
		}
		if err := incrementalVacuum(d); err != nil {
			return err
		}
		current = baseVersion
	}
	steps := migrations()
	if current < baseVersion+len(steps) {
		if err := backup(d, current); err != nil {
			return err
		}
	}
	for i := current - baseVersion; i < len(steps); i++ {
		if err := applyStep(d, baseVersion+1+i, steps[i]); err != nil {
			return err
		}
	}
	return nil
}

// IncrementalVacuum gives the free pages of the database back to the file
// system.
func (s *Store) IncrementalVacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	return err
}

// keepBackups is the count of <path>.v<version>.bak copies that stay.
const keepBackups = 2

// backup copies the database to <path>.v<version>.bak before a migration.
// It then removes all but the keepBackups newest copies.
func backup(d *sql.DB, version int) error {
	var path string
	if err := d.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil || path == "" {
		return err
	}
	dst := fmt.Sprintf("%s.v%d.bak", path, version)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup %s: %w", dst, err)
	}
	if _, err := d.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("backup %s: %w", dst, err)
	}
	return removeOldBackups(path)
}

// GetFlag reads a system_flags value.
func (s *Store) GetFlag(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM system_flags WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetFlag writes a system_flags value.
func (s *Store) SetFlag(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO system_flags (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteFlag removes a system_flags key.
func (s *Store) DeleteFlag(key string) error {
	_, err := s.db.Exec(`DELETE FROM system_flags WHERE key = ?`, key)
	return err
}

// IsDraining reports the persisted drain flag (shared by API, daemon, alerts).
func (s *Store) IsDraining() bool {
	v, ok, err := s.GetFlag("drain")
	return err == nil && ok && v == "1"
}

// Idempotent returns the id an earlier call under scope and key produced, when
// the key is still retained.
func (s *Store) Idempotent(ctx context.Context, scope, key string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT result_id FROM idempotency_keys WHERE scope = ? AND key = ?`, scope, key).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return id, true, nil
}

// RememberIdempotent binds key to resultID. The first writer wins, so a racing
// retry reads the id the winner stored.
func (s *Store) RememberIdempotent(ctx context.Context, scope, key, resultID string) (string, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency_keys (scope, key, result_id, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(scope, key) DO NOTHING`,
		scope, key, resultID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	got, _, err := s.Idempotent(ctx, scope, key)
	if err != nil || got == "" {
		return resultID, err
	}
	return got, nil
}

// PruneIdempotent drops keys older than retention.
func (s *Store) PruneIdempotent(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// JobNameSQL is a select expression for the name of the job or agent that
// idCol refers to. A workspace ID in idCol is the workspace assistant.
func JobNameSQL(idCol string) string {
	return `COALESCE((SELECT ji.name FROM job_index ji WHERE ji.id = ` + idCol + `), ` +
		`CASE WHEN SUBSTR(` + idCol + `, 1, 3) = 'ws_' THEN 'assistant' ELSE '' END)`
}

// JobRefSQL is a condition that matches idCol to one job or agent by ID, or
// by its live name in the workspace of wsCol. It takes the reference twice.
func JobRefSQL(idCol, wsCol string) string {
	return `(` + idCol + ` = ? OR ` + idCol + ` IN (SELECT ji.id FROM job_index ji WHERE ji.workspace_id = ` + wsCol +
		` AND ji.name = ? AND ji.retired_at IS NULL))`
}

// OwnerRefSQL is a condition that matches idCol to an agent (ID or index
// name) or to a job that the agent owns. It takes the reference four times.
func OwnerRefSQL(idCol, wsCol string) string {
	agent := `(SELECT o.id FROM job_index o WHERE o.id = ? OR (o.workspace_id = ` + wsCol +
		` AND o.name = ? AND o.retired_at IS NULL))`
	return `(` + idCol + ` IN ` + agent + ` OR ` + idCol + ` IN (SELECT ji.id FROM job_index ji WHERE ji.owner_id IN ` + agent + `))`
}

// applyStep runs one script and records its version in one transaction.
func applyStep(d *sql.DB, version int, script string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(script); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migration v%d: %w", version, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("recording migration v%d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration v%d: %w", version, err)
	}
	return nil
}

// incrementalVacuum sets auto_vacuum INCREMENTAL. The mode applies only
// after a VACUUM on the same connection.
func incrementalVacuum(d *sql.DB) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // a pooled connection; close returns it
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return fmt.Errorf("auto_vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

// removeOldBackups keeps the keepBackups copies with the highest version.
func removeOldBackups(path string) error {
	matches, err := filepath.Glob(path + ".v*.bak")
	if err != nil {
		return err
	}
	type copyFile struct {
		path    string
		version int
	}
	var copies []copyFile
	for _, m := range matches {
		v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(m, path+".v"), ".bak"))
		if err != nil {
			continue
		}
		copies = append(copies, copyFile{m, v})
	}
	slices.SortFunc(copies, func(a, b copyFile) int { return b.version - a.version })
	for _, c := range copies[min(keepBackups, len(copies)):] {
		if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove backup %s: %w", c.path, err)
		}
	}
	return nil
}
