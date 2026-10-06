package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

// fakeClock is a RunClock for tests.
type fakeClock struct {
	ref RunRef
	ok  bool
}

// testClock is a settable now for tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c fakeClock) LastComplete(context.Context, string, string) (RunRef, bool, error) {
	return c.ref, c.ok, nil
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestPutValidation(t *testing.T) {
	t.Parallel()
	decl := map[string]Collection{}
	cols, err := ParseCollections(map[string]any{"reported": map[string]any{
		"schema": map[string]any{
			"type": "object", "required": []any{"title"},
			"properties": map[string]any{
				"title": map[string]any{"type": "string"},
				"topic": map[string]any{"enum": []any{"alz", "hair"}},
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	decl["reported"] = cols["reported"]
	cases := []struct {
		name string
		coll string
		mode Mode
		rows []Row
		path string
	}{
		{"empty key", "c", "", []Row{{Key: "", Value: raw(`{}`)}}, "key"},
		{"long key", "c", "", []Row{{Key: strings.Repeat("k", 257), Value: raw(`{}`)}}, "key"},
		{"bad utf8", "c", "", []Row{{Key: "a\xff", Value: raw(`{}`)}}, "key"},
		{"control char", "c", "", []Row{{Key: "a\nb", Value: raw(`{}`)}}, "key"},
		{"duplicate key", "c", "", []Row{{Key: "a", Value: raw(`{}`)}, {Key: "a", Value: raw(`{}`)}}, "key"},
		{"bad date", "c", "", []Row{{Key: "a", At: "2026-02-30", Value: raw(`{}`)}}, "at"},
		{"bad at form", "c", "", []Row{{Key: "a", At: "next week", Value: raw(`{}`)}}, "at"},
		{"array value", "c", "", []Row{{Key: "a", Value: raw(`[1]`)}}, "value"},
		{"missing value", "c", "", []Row{{Key: "a"}}, "value"},
		{"too large", "c", "", []Row{{Key: "a", Value: raw(`{"x":"` + strings.Repeat("y", 40000) + `"}`)}}, "value"},
		{"schema enum", "reported", "", []Row{{Key: "a", Value: raw(`{"title":"t","topic":"bone"}`)}}, "value.topic"},
		{"schema required", "reported", "", []Row{{Key: "a", Value: raw(`{"topic":"alz"}`)}}, "value"},
		{"inc outside merge", "c", ModeUpsert, []Row{{Key: "a", Value: raw(`{}`), Inc: map[string]float64{"n": 1}}}, "inc"},
		{"inc not number", "c", ModeMerge, []Row{{Key: "a", Value: raw(`{"n":"x"}`), Inc: map[string]float64{"n": 1}}}, "inc.n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, _ := newTestStore(t, Limits{}, nil)
			o := owner()
			o.Collections = decl
			_, err := s.Put(context.Background(), o, tc.coll, tc.rows, tc.mode)
			var be *BatchError
			if !errors.As(err, &be) {
				t.Fatalf("err = %v, want BatchError", err)
			}
			if be.Rows[0].Path != tc.path {
				t.Fatalf("path = %q (%s), want %q", be.Rows[0].Path, be.Rows[0].Msg, tc.path)
			}
		})
	}
}

func TestPutCollectionName(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	for _, c := range []string{"", "Reported", "1a", strings.Repeat("a", 49), "a b"} {
		if _, err := s.Put(context.Background(), owner(), c, []Row{{Key: "k", Value: raw(`{}`)}}, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("collection %q: err = %v, want ErrInvalid", c, err)
		}
	}
}

func TestPutBatchAllOrNothing(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	_, err := s.Put(ctx, owner(), "c", []Row{
		{Key: "a", Value: raw(`{"x":1}`)},
		{Key: "b", Value: raw(`"no"`)},
		{Key: "c", At: "2026-13-01", Value: raw(`{}`)},
	}, "")
	var be *BatchError
	if !errors.As(err, &be) || len(be.Rows) != 2 || be.Rows[0].Line != 2 || be.Rows[1].Line != 3 {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "2 of 3 rows are not valid") {
		t.Fatalf("message = %q", err.Error())
	}
	got, _ := s.Get(ctx, owner(), "c", []string{"a"})
	if len(got) != 0 {
		t.Fatalf("row a written: %v", got)
	}
}

func TestPutBatchLimits(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{BatchRows: 2, BatchBytes: 100}, nil)
	ctx := context.Background()
	rows := []Row{{Key: "a", Value: raw(`{}`)}, {Key: "b", Value: raw(`{}`)}, {Key: "c", Value: raw(`{}`)}}
	var be *BatchError
	if _, err := s.Put(ctx, owner(), "c", rows, ""); !errors.As(err, &be) || be.Rows[0].Path != "batch" {
		t.Fatalf("rows limit: %v", err)
	}
	big := []Row{{Key: "a", Value: raw(`{"x":"` + strings.Repeat("y", 200) + `"}`)}}
	if _, err := s.Put(ctx, owner(), "c", big, ""); !errors.As(err, &be) || be.Rows[0].Path != "batch" {
		t.Fatalf("bytes limit: %v", err)
	}
}

func TestPutOwnerLimits(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{OwnerRows: 2, OwnerBytes: 200}, nil)
	ctx := context.Background()
	mustPut(t, s, owner(), "c", "", Row{Key: "a", Value: raw(`{}`)}, Row{Key: "b", Value: raw(`{}`)})
	var be *BatchError
	if _, err := s.Put(ctx, owner(), "d", []Row{{Key: "c", Value: raw(`{}`)}}, ""); !errors.As(err, &be) || be.Rows[0].Path != "owner" {
		t.Fatalf("rows: %v", err)
	}
	if _, err := s.Put(ctx, owner(), "c", []Row{{Key: "a", Value: raw(`{"x":"` + strings.Repeat("y", 300) + `"}`)}}, ""); !errors.As(err, &be) || be.Rows[0].Path != "owner" {
		t.Fatalf("bytes: %v", err)
	}
	other := Owner{ID: "jb_0000000002"}
	mustPut(t, s, other, "c", "", Row{Key: "a", Value: raw(`{}`)})
}

func TestPutModes(t *testing.T) {
	t.Parallel()
	s, _, tc := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	o := owner()

	res := mustPut(t, s, o, "c", ModeInsert, Row{Key: "a", Value: raw(`{"n":1,"m":{"x":1}}`)})
	if res.Inserted != 1 {
		t.Fatalf("insert: %+v", res)
	}
	var be *BatchError
	if _, err := s.Put(ctx, o, "c", []Row{{Key: "a", Value: raw(`{}`)}}, ModeInsert); !errors.As(err, &be) || be.Rows[0].Path != "key" {
		t.Fatalf("insert over existing: %v", err)
	}

	tc.advance(time.Minute)
	res = mustPut(t, s, o, "c", ModeMerge, Row{Key: "a", Value: raw(`{"m":{"y":2},"z":null}`), Inc: map[string]float64{"n": 2, "hits": 1}})
	if res.Updated != 1 {
		t.Fatalf("merge: %+v", res)
	}
	got, _ := s.Get(ctx, o, "c", []string{"a"})
	if string(got[0].Value) != `{"hits":1,"m":{"x":1,"y":2},"n":3}` {
		t.Fatalf("merged value = %s", got[0].Value)
	}
	if got[0].CreatedAt == got[0].UpdatedAt {
		t.Fatalf("created_at changed: %+v", got[0])
	}

	res = mustPut(t, s, o, "c", ModeMerge, Row{Key: "new", Inc: map[string]float64{"n": 5}})
	if res.Inserted != 1 {
		t.Fatalf("merge insert: %+v", res)
	}

	res = mustPut(t, s, o, "c", ModeUpsert, Row{Key: "a", Value: raw(`{"only":true}`)})
	if res.Updated != 1 {
		t.Fatalf("upsert: %+v", res)
	}
	got, _ = s.Get(ctx, o, "c", []string{"a"})
	if string(got[0].Value) != `{"only":true}` {
		t.Fatalf("upsert value = %s", got[0].Value)
	}
}

func TestPutUnchanged(t *testing.T) {
	t.Parallel()
	s, _, tc := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	o := owner()
	mustPut(t, s, o, "c", "", Row{Key: "a", At: "2026-09-01", Value: raw(`{"b":1,"a":2}`)})
	before, _ := s.Get(ctx, o, "c", []string{"a"})
	tc.advance(time.Hour)
	res := mustPut(t, s, o, "c", "", Row{Key: "a", Value: raw(`{"a":2, "b":1}`)})
	if res.Unchanged != 1 || res.Updated != 0 {
		t.Fatalf("res = %+v", res)
	}
	after, _ := s.Get(ctx, o, "c", []string{"a"})
	if after[0].UpdatedAt != before[0].UpdatedAt || after[0].At != "2026-09-01T00:00:00Z" {
		t.Fatalf("before %+v after %+v", before[0], after[0])
	}
}

func TestPutAtTimeZone(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	o := owner()
	o.Loc = loc
	mustPut(t, s, o, "c", "",
		Row{Key: "d", At: "2026-09-01", Value: raw(`{}`)},
		Row{Key: "m", At: "2026-09-01 10:30", Value: raw(`{}`)},
		Row{Key: "r", At: "2026-09-01T10:30:00-05:00", Value: raw(`{}`)})
	got, _ := s.Get(ctx, o, "c", []string{"d", "m", "r"})
	want := []string{"2026-08-31T22:00:00Z", "2026-09-01T08:30:00Z", "2026-09-01T15:30:00Z"}
	for i, r := range got {
		if r.At != want[i] {
			t.Errorf("%s at = %s, want %s", r.Key, r.At, want[i])
		}
	}
}

func TestGetHasDelete(t *testing.T) {
	t.Parallel()
	s, db, _ := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	o := owner()
	o.RunID = "run1"
	mustPut(t, s, o, "c", "", Row{Key: "a", At: "2026-09-01", Value: raw(`{"t":"x"}`)}, Row{Key: "b", Value: raw(`{}`)})
	got, err := s.Get(ctx, o, "c", []string{"b", "zz", "a"})
	if err != nil || len(got) != 2 || got[0].Key != "b" || got[1].Key != "a" {
		t.Fatalf("get = %v, %v", got, err)
	}
	has, _ := s.Has(ctx, o, "c", []string{"a", "b", "zz"})
	if has["a"] != "2026-09-01T00:00:00Z" || has["b"] == "" || len(has) != 2 {
		t.Fatalf("has = %v", has)
	}
	n, err := s.Delete(ctx, o, "c", []string{"a", "zz"})
	if err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
	var ins, del int
	if err := db.QueryRow(`SELECT inserted, deleted FROM state_run_stats WHERE run_id = 'run1'`).Scan(&ins, &del); err != nil || ins != 2 || del != 1 {
		t.Fatalf("stats = %d %d %v", ins, del, err)
	}
	ftsCheck(t, db)
}

func TestPruneDrop(t *testing.T) {
	t.Parallel()
	s, db, _ := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	o := owner()
	mustPut(t, s, o, "c", "",
		Row{Key: "old", At: "2026-01-01", Value: raw(`{"t":"one"}`)},
		Row{Key: "new", At: "2026-10-01", Value: raw(`{"t":"two"}`)})
	if _, err := s.Prune(ctx, o, "c", TimeRange{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("prune with no range: %v", err)
	}
	r, err := s.Range(ctx, o, "", "180d", "")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, o, "c", r)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	n, err = s.Drop(ctx, o, "c")
	if err != nil || n != 1 {
		t.Fatalf("drop = %d, %v", n, err)
	}
	ftsCheck(t, db)
}

func TestTTLAndSweep(t *testing.T) {
	t.Parallel()
	s, db, tc := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	o := owner()
	cols, err := ParseCollections(map[string]any{"short": map[string]any{"ttl": "1d"}})
	if err != nil {
		t.Fatal(err)
	}
	o.Collections = cols
	mustPut(t, s, o, "short", "", Row{Key: "a", Value: raw(`{"t":"gone"}`)})
	mustPut(t, s, o, "keep", "", Row{Key: "a", Value: raw(`{}`)})
	tc.advance(25 * time.Hour)
	if got, _ := s.Get(ctx, o, "short", []string{"a"}); len(got) != 0 {
		t.Fatalf("expired row is visible: %v", got)
	}
	res := mustPut(t, s, o, "short", ModeInsert, Row{Key: "a", Value: raw(`{"t":"again"}`)})
	if res.Inserted != 1 {
		t.Fatalf("insert over expired: %+v", res)
	}
	tc.advance(25 * time.Hour)
	expired, purged, err := s.Sweep(ctx)
	if err != nil || expired != 1 || purged != 0 {
		t.Fatalf("sweep = %d %d %v", expired, purged, err)
	}
	ftsCheck(t, db)
}

func TestSweepRetired(t *testing.T) {
	t.Parallel()
	s, db, tc := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	mustPut(t, s, owner(), "c", "", Row{Key: "a", Value: raw(`{"t":"x"}`)})
	mustPut(t, s, Owner{ID: "jb_live"}, "c", "", Row{Key: "a", Value: raw(`{}`)})
	for _, q := range []string{
		`INSERT INTO job_index (workspace_id, name, path, updated_at, id, retired_at) VALUES
		 ('ws_1', 'gone', '/x', 't', 'jb_0000000001', '2026-08-01T00:00:00Z'),
		 ('ws_1', 'live', '/y', 't', 'jb_live', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tc.advance(time.Hour)
	_, purged, err := s.Sweep(ctx)
	if err != nil || purged != 1 {
		t.Fatalf("sweep purged = %d, %v", purged, err)
	}
	if got, _ := s.Get(ctx, Owner{ID: "jb_live"}, "c", []string{"a"}); len(got) != 1 {
		t.Fatal("live owner lost its row")
	}
	ftsCheck(t, db)
}

func TestFTSFollowsWrites(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	ctx := context.Background()
	o := owner()
	find := func(q string) string {
		t.Helper()
		res, err := s.Query(ctx, o, Query{Collection: "c", Q: q})
		if err != nil {
			t.Fatal(err)
		}
		return keysOf(res)
	}
	mustPut(t, s, o, "c", "", Row{Key: "a", Value: raw(`{"t":"alpha","n":{"deep":["delta"]}}`)})
	if find("delta") != "a" {
		t.Fatal("nested string is not in the index")
	}
	mustPut(t, s, o, "c", "", Row{Key: "a", Value: raw(`{"t":"beta"}`)})
	if find("alpha") != "" || find("beta") != "a" {
		t.Fatal("update left old tokens")
	}
	if _, err := s.Delete(ctx, o, "c", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, o, "c", "", Row{Key: "b", Value: raw(`{"t":"gamma"}`)})
	if find("beta") != "" || find("gamma") != "b" {
		t.Fatal("delete left old tokens")
	}
}

func newTestStore(t *testing.T, limits Limits, clock RunClock) (*Store, *sql.DB, *testClock) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tc := &testClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	return NewStore(st.DB(), limits, clock, tc.now), st.DB(), tc
}

func owner() Owner { return Owner{ID: "jb_0000000001", WorkspaceID: "ws_1"} }

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func mustPut(t *testing.T, s *Store, o Owner, coll string, mode Mode, rows ...Row) PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), o, coll, rows, mode)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return res
}

// ftsCheck fails when state_fts has a token of a row that is gone.
// A contentless table keeps such a token when a delete gives the wrong values.
func ftsCheck(t *testing.T, db *sql.DB) {
	t.Helper()
	var stale int
	err := db.QueryRow(`SELECT COUNT(*) FROM state_fts f WHERE state_fts MATCH 'gone OR one OR two OR old OR x'
AND NOT EXISTS (SELECT 1 FROM state_rows r WHERE r.rowid = f.rowid)`).Scan(&stale)
	if err != nil || stale != 0 {
		t.Fatalf("stale fts rows = %d, %v", stale, err)
	}
}
