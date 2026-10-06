// Package state keeps the records of a job or an agent: collections of keyed
// JSON rows with times, full-text search, and JS filters (plan 2026-10-06 Part B).
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

// Store reads and writes state rows. It owns state_rows, state_fts, and
// state_run_stats.
type Store struct {
	db     *sql.DB
	limits Limits
	clock  RunClock
	now    func() time.Time
}

// Limits caps rows, bytes, and scans. A zero field takes its default.
type Limits struct {
	RowBytes   int   // bytes of one value
	OwnerRows  int   // config defaults.state.max_rows
	OwnerBytes int64 // config defaults.state.max_bytes
	BatchRows  int   // rows in one Put
	BatchBytes int   // bytes in one Put
	ScanRows   int   // rows that a JS filter or a value sort reads
}

// Owner is the scope of one call. RunID is empty outside a run.
type Owner struct {
	ID          string // jb_…, agt_…, or ws_…
	WorkspaceID string
	RunID       string
	Collections map[string]Collection // declared in the frontmatter; may be nil
	Loc         *time.Location        // workspace time zone for dates with no time; nil = UTC
}

type Row struct {
	Key       string             `json:"key"`
	At        string             `json:"at,omitempty"`
	Value     json.RawMessage    `json:"value,omitempty"`
	Inc       map[string]float64 `json:"inc,omitempty"` // merge mode only
	CreatedAt string             `json:"created_at,omitempty"`
	UpdatedAt string             `json:"updated_at,omitempty"`
}

// Mode is the write mode of Put.
type Mode string

type PutResult struct {
	Inserted  int `json:"inserted"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// RowError is one failed check. Line 0 is a check on the whole batch or owner.
type RowError struct {
	Line int    `json:"line"` // 1-based index in the batch
	Path string `json:"path"` // "key", "at", "value.topic", ...
	Msg  string `json:"msg"`
}

// BatchError is the result of a Put with one or more failed checks. Put
// writes nothing when it returns a BatchError.
type BatchError struct {
	Rows  []RowError
	Total int
}

// stored is one row as the table keeps it.
type stored struct {
	rowid   int64
	key     string
	value   string
	at      sql.NullString
	created string
	expires sql.NullString
}

const (
	ModeUpsert Mode = "upsert"
	ModeInsert Mode = "insert"
	ModeMerge  Mode = "merge"

	retiredGrace = 30 * 24 * time.Hour
	sqlChunk     = 500
	liveSQL      = "(expires_at IS NULL OR expires_at > ?)"
)

// ErrInvalid marks a request that is not valid (collection name, time, cursor, filter).
var ErrInvalid = errors.New("invalid state request")

// DefaultLimits returns the plan defaults.
func DefaultLimits() Limits {
	return Limits{
		RowBytes:   32 << 10,
		OwnerRows:  100000,
		OwnerBytes: 50 << 20,
		BatchRows:  1000,
		BatchBytes: 4 << 20,
		ScanRows:   10000,
	}
}

// NewStore builds a Store. clock may be nil; then "last-run" fails and the
// brief shows no last run. now may be nil (time.Now).
func NewStore(db *sql.DB, limits Limits, clock RunClock, now func() time.Time) *Store {
	d := DefaultLimits()
	if limits.RowBytes <= 0 {
		limits.RowBytes = d.RowBytes
	}
	if limits.OwnerRows <= 0 {
		limits.OwnerRows = d.OwnerRows
	}
	if limits.OwnerBytes <= 0 {
		limits.OwnerBytes = d.OwnerBytes
	}
	if limits.BatchRows <= 0 {
		limits.BatchRows = d.BatchRows
	}
	if limits.BatchBytes <= 0 {
		limits.BatchBytes = d.BatchBytes
	}
	if limits.ScanRows <= 0 {
		limits.ScanRows = d.ScanRows
	}
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, limits: limits, clock: clock, now: now}
}

func (e *BatchError) Error() string {
	bad := map[int]bool{}
	for _, r := range e.Rows {
		if r.Line > 0 {
			bad[r.Line] = true
		}
	}
	if len(bad) == 0 && len(e.Rows) > 0 {
		return e.Rows[0].Path + ": " + e.Rows[0].Msg + ", nothing written"
	}
	return fmt.Sprintf("%d of %d rows are not valid, nothing written", len(bad), e.Total)
}

// Put checks all rows, then writes all of them in one transaction, or none.
func (s *Store) Put(ctx context.Context, o Owner, coll string, rows []Row, mode Mode) (PutResult, error) {
	if err := checkCollection(coll); err != nil {
		return PutResult{}, err
	}
	if mode == "" {
		mode = ModeUpsert
	}
	if mode != ModeUpsert && mode != ModeInsert && mode != ModeMerge {
		return PutResult{}, fmt.Errorf("%w: mode %q: use upsert, insert, or merge", ErrInvalid, mode)
	}
	b := newBatch(o, coll, mode, s.limits, s.now())
	if errs := b.size(rows); len(errs) > 0 {
		return PutResult{}, &BatchError{Rows: errs, Total: len(rows)}
	}
	var res PutResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		keys := make([]string, len(rows))
		for i, r := range rows {
			keys[i] = r.Key
		}
		existing, err := s.load(ctx, tx, o.ID, coll, keys)
		if err != nil {
			return err
		}
		writes, errs := b.plan(rows, existing)
		if len(errs) > 0 {
			return &BatchError{Rows: errs, Total: len(rows)}
		}
		if res, err = s.apply(ctx, tx, o, coll, b, writes); err != nil {
			return err
		}
		if err := s.checkOwner(ctx, tx, o.ID, len(rows)); err != nil {
			return err
		}
		return s.addStats(ctx, tx, o, coll, res.Inserted, res.Updated, 0)
	})
	if err != nil {
		return PutResult{}, err
	}
	return res, nil
}

// Get returns the live rows of keys, in the order of keys. A missing key has no row.
func (s *Store) Get(ctx context.Context, o Owner, coll string, keys []string) ([]Row, error) {
	if err := checkCollection(coll); err != nil {
		return nil, err
	}
	found := map[string]Row{}
	now := formatTS(s.now())
	for _, part := range chunks(keys) {
		q := `SELECT key, value, at, created_at, updated_at FROM state_rows
WHERE owner_id = ? AND collection = ? AND ` + liveSQL + ` AND key IN (` + marks(len(part)) + `)`
		rows, err := s.db.QueryContext(ctx, q, append([]any{o.ID, coll, now}, toAny(part)...)...)
		if err != nil {
			return nil, fmt.Errorf("state get: %w", err)
		}
		for rows.Next() {
			var r Row
			var value string
			var at sql.NullString
			if err := rows.Scan(&r.Key, &value, &at, &r.CreatedAt, &r.UpdatedAt); err != nil {
				_ = rows.Close() // the scan error wins
				return nil, fmt.Errorf("state get: %w", err)
			}
			r.Value, r.At = json.RawMessage(value), at.String
			found[r.Key] = r
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("state get: %w", err)
		}
	}
	out := make([]Row, 0, len(found))
	for _, k := range keys {
		if r, ok := found[k]; ok {
			out = append(out, r)
			delete(found, k)
		}
	}
	return out, nil
}

// Has maps each found key to COALESCE(at, created_at). A missing key is absent.
func (s *Store) Has(ctx context.Context, o Owner, coll string, keys []string) (map[string]string, error) {
	if err := checkCollection(coll); err != nil {
		return nil, err
	}
	out := map[string]string{}
	now := formatTS(s.now())
	for _, part := range chunks(keys) {
		q := `SELECT key, COALESCE(at, created_at) FROM state_rows
WHERE owner_id = ? AND collection = ? AND ` + liveSQL + ` AND key IN (` + marks(len(part)) + `)`
		rows, err := s.db.QueryContext(ctx, q, append([]any{o.ID, coll, now}, toAny(part)...)...)
		if err != nil {
			return nil, fmt.Errorf("state has: %w", err)
		}
		for rows.Next() {
			var k, at string
			if err := rows.Scan(&k, &at); err != nil {
				_ = rows.Close() // the scan error wins
				return nil, fmt.Errorf("state has: %w", err)
			}
			out[k] = at
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("state has: %w", err)
		}
	}
	return out, nil
}

// Delete removes the rows of keys and returns the count.
func (s *Store) Delete(ctx context.Context, o Owner, coll string, keys []string) (int, error) {
	if err := checkCollection(coll); err != nil {
		return 0, err
	}
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, part := range chunks(keys) {
			d, err := s.remove(ctx, tx, `owner_id = ? AND collection = ? AND key IN (`+marks(len(part))+`)`,
				append([]any{o.ID, coll}, toAny(part)...)...)
			if err != nil {
				return err
			}
			n += d
		}
		return s.addStats(ctx, tx, o, coll, 0, 0, n)
	})
	return n, err
}

// Prune removes the rows in r. Since or Until must be set.
func (s *Store) Prune(ctx context.Context, o Owner, coll string, r TimeRange) (int, error) {
	if err := checkCollection(coll); err != nil {
		return 0, err
	}
	col, err := r.column()
	if err != nil {
		return 0, err
	}
	if r.Since.IsZero() && r.Until.IsZero() {
		return 0, fmt.Errorf("%w: prune needs since or until", ErrInvalid)
	}
	where := `owner_id = ? AND collection = ?`
	args := []any{o.ID, coll}
	if !r.Since.IsZero() {
		where += ` AND ` + col + ` >= ?`
		args = append(args, formatTS(r.Since))
	}
	if !r.Until.IsZero() {
		where += ` AND ` + col + ` < ?`
		args = append(args, formatTS(r.Until))
	}
	n := 0
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if n, err = s.remove(ctx, tx, where, args...); err != nil {
			return err
		}
		return s.addStats(ctx, tx, o, coll, 0, 0, n)
	})
	return n, err
}

// Drop removes all rows of a collection.
func (s *Store) Drop(ctx context.Context, o Owner, coll string) (int, error) {
	if err := checkCollection(coll); err != nil {
		return 0, err
	}
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if n, err = s.remove(ctx, tx, `owner_id = ? AND collection = ?`, o.ID, coll); err != nil {
			return err
		}
		return s.addStats(ctx, tx, o, coll, 0, 0, n)
	})
	return n, err
}

// Sweep removes rows past expires_at, and the rows of owners retired for more
// than 30 days.
func (s *Store) Sweep(ctx context.Context) (expired, purged int, err error) {
	now := s.now()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if expired, err = s.remove(ctx, tx, `expires_at IS NOT NULL AND expires_at <= ?`, formatTS(now)); err != nil {
			return err
		}
		const retired = `owner_id IN (SELECT id FROM job_index WHERE retired_at IS NOT NULL AND retired_at < ?)`
		cutoff := formatTS(now.Add(-retiredGrace))
		if purged, err = s.remove(ctx, tx, retired, cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_run_stats WHERE `+retired, cutoff); err != nil {
			return fmt.Errorf("state sweep stats: %w", err)
		}
		return nil
	})
	return expired, purged, err
}

func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // fn error wins
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state commit: %w", err)
	}
	return nil
}

// load reads the stored rows of keys, live or expired.
func (s *Store) load(ctx context.Context, tx *sql.Tx, owner, coll string, keys []string) (map[string]stored, error) {
	out := map[string]stored{}
	for _, part := range chunks(keys) {
		q := `SELECT rowid, key, value, at, created_at, expires_at FROM state_rows
WHERE owner_id = ? AND collection = ? AND key IN (` + marks(len(part)) + `)`
		rows, err := tx.QueryContext(ctx, q, append([]any{owner, coll}, toAny(part)...)...)
		if err != nil {
			return nil, fmt.Errorf("state load: %w", err)
		}
		for rows.Next() {
			var r stored
			if err := rows.Scan(&r.rowid, &r.key, &r.value, &r.at, &r.created, &r.expires); err != nil {
				_ = rows.Close() // the scan error wins
				return nil, fmt.Errorf("state load: %w", err)
			}
			out[r.key] = r
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("state load: %w", err)
		}
	}
	return out, nil
}

// apply writes the planned rows and keeps state_fts in step.
func (s *Store) apply(ctx context.Context, tx *sql.Tx, o Owner, coll string, b *batch, writes []write) (PutResult, error) {
	var res PutResult
	now := formatTS(b.now)
	expires := b.expires()
	runID := store.NullStr(o.RunID)
	for _, w := range writes {
		live := w.old != nil && w.old.live(b.now)
		switch {
		case live && w.old.value == w.value && w.old.at == w.at:
			res.Unchanged++
			continue
		case w.old == nil:
			r, err := tx.ExecContext(ctx, `INSERT INTO state_rows
(owner_id, collection, key, value, at, created_at, updated_at, expires_at, run_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.ID, coll, w.key, w.value, w.at, now, now, expires, runID)
			if err != nil {
				return res, fmt.Errorf("state insert %q: %w", w.key, err)
			}
			id, err := r.LastInsertId()
			if err != nil {
				return res, fmt.Errorf("state insert %q: %w", w.key, err)
			}
			if err := s.index(ctx, tx, id, w.key, w.value); err != nil {
				return res, err
			}
			res.Inserted++
			continue
		}
		created := w.old.created
		if !live {
			created = now
		}
		if err := s.unindex(ctx, tx, w.old.rowid, w.old.key, w.old.value); err != nil {
			return res, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE state_rows SET value = ?, at = ?, created_at = ?,
updated_at = ?, expires_at = ?, run_id = ? WHERE rowid = ?`, w.value, w.at, created, now, expires, runID, w.old.rowid); err != nil {
			return res, fmt.Errorf("state update %q: %w", w.key, err)
		}
		if err := s.index(ctx, tx, w.old.rowid, w.key, w.value); err != nil {
			return res, err
		}
		if live {
			res.Updated++
		} else {
			res.Inserted++
		}
	}
	return res, nil
}

// checkOwner fails the Put when the owner is over its row or byte limit.
func (s *Store) checkOwner(ctx context.Context, tx *sql.Tx, owner string, total int) error {
	var rows int
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB))), 0)
FROM state_rows WHERE owner_id = ?`, owner).Scan(&rows, &size); err != nil {
		return fmt.Errorf("state owner size: %w", err)
	}
	switch {
	case rows > s.limits.OwnerRows:
		return &BatchError{Total: total, Rows: []RowError{{Path: "owner", Msg: fmt.Sprintf(
			"the owner would have %d rows, limit %d; delete or prune rows first", rows, s.limits.OwnerRows)}}}
	case size > s.limits.OwnerBytes:
		return &BatchError{Total: total, Rows: []RowError{{Path: "owner", Msg: fmt.Sprintf(
			"the owner would have %d bytes, limit %d; delete or prune rows first", size, s.limits.OwnerBytes)}}}
	}
	return nil
}

// addStats adds counts to state_run_stats for the run of o.
func (s *Store) addStats(ctx context.Context, tx *sql.Tx, o Owner, coll string, ins, upd, del int) error {
	if o.RunID == "" || ins+upd+del == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO state_run_stats (owner_id, run_id, collection, inserted, updated, deleted)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner_id, run_id, collection) DO UPDATE SET
inserted = state_run_stats.inserted + excluded.inserted,
updated = state_run_stats.updated + excluded.updated,
deleted = state_run_stats.deleted + excluded.deleted`, o.ID, o.RunID, coll, ins, upd, del)
	if err != nil {
		return fmt.Errorf("state run stats: %w", err)
	}
	return nil
}

// remove deletes the rows that match where, and their state_fts entries.
func (s *Store) remove(ctx context.Context, tx *sql.Tx, where string, args ...any) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, key, value FROM state_rows WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("state remove: %w", err)
	}
	var gone []stored
	for rows.Next() {
		var r stored
		if err := rows.Scan(&r.rowid, &r.key, &r.value); err != nil {
			_ = rows.Close() // the scan error wins
			return 0, fmt.Errorf("state remove: %w", err)
		}
		gone = append(gone, r)
	}
	if err := closeRows(rows); err != nil {
		return 0, fmt.Errorf("state remove: %w", err)
	}
	for _, r := range gone {
		if err := s.unindex(ctx, tx, r.rowid, r.key, r.value); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_rows WHERE rowid = ?`, r.rowid); err != nil {
			return 0, fmt.Errorf("state remove: %w", err)
		}
	}
	return len(gone), nil
}

func (s *Store) index(ctx context.Context, tx *sql.Tx, rowid int64, key, value string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_fts (rowid, key, body) VALUES (?, ?, ?)`, rowid, key, ftsBody(value)); err != nil {
		return fmt.Errorf("state fts: %w", err)
	}
	return nil
}

// unindex removes a contentless FTS entry. It needs the old values.
func (s *Store) unindex(ctx context.Context, tx *sql.Tx, rowid int64, key, value string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_fts (state_fts, rowid, key, body) VALUES ('delete', ?, ?, ?)`,
		rowid, key, ftsBody(value)); err != nil {
		return fmt.Errorf("state fts delete: %w", err)
	}
	return nil
}

func (r stored) live(now time.Time) bool {
	return !r.expires.Valid || r.expires.String > formatTS(now)
}

func checkCollection(coll string) error {
	if !collectionRE.MatchString(coll) {
		return fmt.Errorf("%w: collection %q: name must match %s", ErrInvalid, coll, collectionRE)
	}
	return nil
}

// ftsBody joins all string leaves of a JSON value. Map keys are walked in
// order, so one value always gives the same body.
func ftsBody(value string) string {
	var v any
	if err := json.Unmarshal([]byte(value), &v); err != nil {
		return ""
	}
	var parts []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(v)
	return strings.Join(parts, "\n")
}

func chunks(keys []string) [][]string {
	var out [][]string
	for len(keys) > 0 {
		n := min(len(keys), sqlChunk)
		out = append(out, keys[:n])
		keys = keys[n:]
	}
	return out
}

func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		_ = rows.Close() // the iteration error wins
		return err
	}
	return rows.Close()
}
