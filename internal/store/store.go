// Package store owns SQLite open, migrations, and low-level helpers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	_ "modernc.org/sqlite"
)

// Store is a trebi database handle.
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex // serializes bus appends (gapless seq)
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
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", path)
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
	return &Store{db: d, path: path}, nil
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

// LockBus serializes event_log inserts so seq is gapless-in-commit-order.
func (s *Store) LockBus()   { s.mu.Lock() }
func (s *Store) UnlockBus() { s.mu.Unlock() }

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
	steps := migrations()
	for i := current; i < len(steps); i++ {
		version := i + 1
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(steps[i]); err != nil {
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
	}
	return nil
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
