package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

type Query struct {
	Collection string   `json:"collection"`
	Keys       []string `json:"keys,omitempty"`
	Prefix     string   `json:"prefix,omitempty"`
	Q          string   `json:"q,omitempty"`     // FTS5 match on key and string fields
	Where      string   `json:"where,omitempty"` // JS on v, k, at; returns bool
	Since      string   `json:"since,omitempty"`
	Until      string   `json:"until,omitempty"`
	By         string   `json:"by,omitempty"`
	Sort       string   `json:"sort,omitempty"`   // "at", "-at", "updated", "created", "key", "-v.score"
	Fields     []string `json:"fields,omitempty"` // "key", "at", "title" (= v.title), "v.a.b"
	Limit      int      `json:"limit,omitempty"`  // default 50, max 500
	Cursor     string   `json:"cursor,omitempty"`
}

type QueryResult struct {
	Rows      []map[string]any `json:"rows"`
	Next      string           `json:"next,omitempty"`
	Scanned   int              `json:"scanned"`
	Truncated bool             `json:"truncated,omitempty"` // the JS scan cap stopped the query
}

// hit is one row that a query reads.
type hit struct {
	key     string
	at      string
	created string
	updated string
	value   map[string]any
}

// order is a parsed Sort. path is set for a sort on a value field.
type order struct {
	sql  string
	path []string
	desc bool
}

// queryPlan is the parsed sort, page, and filter of a Query.
type queryPlan struct {
	ord    order
	prog   *goja.Program
	limit  int
	offset int
}

const (
	defaultLimit = 50
	maxLimit     = 500
	whereTimeout = 2 * time.Second
	cutRunes     = 300
	cursorPrefix = "o"
)

// Query runs SQL filters, then Where on at most ScanRows rows, then the
// sort, then the page and the fields.
func (s *Store) Query(ctx context.Context, o Owner, q Query) (QueryResult, error) {
	p, err := q.plan()
	if err != nil {
		return QueryResult{}, err
	}
	where, args, err := s.selection(ctx, o, q)
	if err != nil {
		return QueryResult{}, err
	}
	if p.prog == nil && p.ord.path == nil {
		hits, err := s.fetch(ctx, where, args, p.ord.sql, p.limit+1, p.offset)
		if err != nil {
			return QueryResult{}, err
		}
		return q.page(hits, min(len(hits), p.limit), false, 0, p.limit, p.offset), nil
	}
	sqlOrder := p.ord.sql
	if p.ord.path != nil {
		sqlOrder = "key ASC"
	}
	hits, err := s.fetch(ctx, where, args, sqlOrder, s.limits.ScanRows+1, 0)
	if err != nil {
		return QueryResult{}, err
	}
	truncated := len(hits) > s.limits.ScanRows
	if truncated {
		hits = hits[:s.limits.ScanRows]
	}
	scanned := len(hits)
	if p.prog != nil {
		if hits, err = filter(ctx, p.prog, hits); err != nil {
			return QueryResult{}, err
		}
	}
	if p.ord.path != nil {
		p.ord.sortHits(hits)
	}
	return q.page(hits, scanned, truncated, p.offset, p.limit, p.offset), nil
}

// Range parses since, until, and by. "last-run" uses the RunClock.
func (s *Store) Range(ctx context.Context, o Owner, since, until, by string) (TimeRange, error) {
	r := TimeRange{By: by}
	if _, err := r.column(); err != nil {
		return r, err
	}
	var err error
	if since != "" {
		if r.Since, err = s.parseTime(ctx, o, since); err != nil {
			return r, err
		}
	}
	if until != "" {
		if r.Until, err = s.parseTime(ctx, o, until); err != nil {
			return r, err
		}
	}
	return r, nil
}

func (s *Store) parseTime(ctx context.Context, o Owner, v string) (time.Time, error) {
	var clockErr error
	t, err := ParseTime(v, s.now(), o.Loc, func() (time.Time, bool) {
		if s.clock == nil {
			return time.Time{}, false
		}
		ref, ok, err := s.clock.LastComplete(ctx, o.ID, o.RunID)
		if err != nil {
			clockErr = err
			return time.Time{}, false
		}
		return ref.Start, ok
	})
	if clockErr != nil {
		return time.Time{}, fmt.Errorf("state last run: %w", clockErr)
	}
	return t, err
}

// selection builds the SQL filter of q.
func (s *Store) selection(ctx context.Context, o Owner, q Query) (string, []any, error) {
	where := []string{"owner_id = ?", "collection = ?", liveSQL}
	args := []any{o.ID, q.Collection, formatTS(s.now())}
	if len(q.Keys) > 0 {
		where = append(where, "key IN ("+marks(len(q.Keys))+")")
		args = append(args, toAny(q.Keys)...)
	}
	if q.Prefix != "" {
		where = append(where, "substr(key, 1, ?) = ?")
		args = append(args, utf8.RuneCountInString(q.Prefix), q.Prefix)
	}
	if q.Since != "" || q.Until != "" {
		r, err := s.Range(ctx, o, q.Since, q.Until, q.By)
		if err != nil {
			return "", nil, err
		}
		col, _ := r.column() // Range checked By
		if !r.Since.IsZero() {
			where = append(where, col+" >= ?")
			args = append(args, formatTS(r.Since))
		}
		if !r.Until.IsZero() {
			where = append(where, col+" < ?")
			args = append(args, formatTS(r.Until))
		}
	}
	if m := ftsQuery(q.Q); m != "" {
		where = append(where, "rowid IN (SELECT rowid FROM state_fts WHERE state_fts MATCH ?)")
		args = append(args, m)
	}
	return strings.Join(where, " AND "), args, nil
}

func (s *Store) fetch(ctx context.Context, where string, args []any, orderBy string, limit, offset int) ([]hit, error) {
	q := `SELECT key, value, COALESCE(at, ''), created_at, updated_at FROM state_rows WHERE ` + where +
		` ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("state query: %w", err)
	}
	var out []hit
	for rows.Next() {
		var h hit
		var value string
		if err := rows.Scan(&h.key, &value, &h.at, &h.created, &h.updated); err != nil {
			_ = rows.Close() // the scan error wins
			return nil, fmt.Errorf("state query: %w", err)
		}
		if err := json.Unmarshal([]byte(value), &h.value); err != nil {
			h.value = map[string]any{}
		}
		out = append(out, h)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("state query: %w", err)
	}
	return out, nil
}

// plan checks q and parses its sort, cursor, limit, and Where.
func (q Query) plan() (queryPlan, error) {
	var p queryPlan
	if err := checkCollection(q.Collection); err != nil {
		return p, err
	}
	var err error
	if p.ord, err = parseOrder(q.Sort); err != nil {
		return p, err
	}
	if p.offset, err = parseCursor(q.Cursor); err != nil {
		return p, err
	}
	p.limit = q.Limit
	if p.limit <= 0 {
		p.limit = defaultLimit
	}
	p.limit = min(p.limit, maxLimit)
	if q.Where != "" {
		if p.prog, err = goja.Compile("where", "("+q.Where+")", false); err != nil {
			return p, fmt.Errorf("%w: where: %v", ErrInvalid, err)
		}
	}
	return p, nil
}

// page cuts one page from hits. skip is the count of hits to drop first.
func (q Query) page(hits []hit, scanned int, truncated bool, skip, limit, offset int) QueryResult {
	res := QueryResult{Rows: []map[string]any{}, Scanned: scanned, Truncated: truncated}
	if skip < len(hits) {
		hits = hits[skip:]
	} else {
		hits = nil
	}
	if len(hits) > limit {
		hits = hits[:limit]
		res.Next = base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(offset+limit)))
	}
	for _, h := range hits {
		res.Rows = append(res.Rows, q.project(h))
	}
	return res
}

// project returns the output map of one row.
func (q Query) project(h hit) map[string]any {
	meta := map[string]any{"key": h.key, "at": h.at, "created_at": h.created, "updated_at": h.updated}
	if h.at == "" {
		meta["at"] = nil
	}
	if len(q.Fields) == 0 {
		out := make(map[string]any, len(h.value)+2)
		for k, v := range h.value {
			out[k] = cut(v)
		}
		out["key"] = h.key
		if h.at != "" {
			out["at"] = h.at
		} else {
			delete(out, "at")
		}
		return out
	}
	out := make(map[string]any, len(q.Fields))
	for _, f := range q.Fields {
		if v, ok := meta[f]; ok {
			out[f] = v
			continue
		}
		v, _ := lookup(h.value, strings.Split(strings.TrimPrefix(f, "v."), "."))
		out[f] = v
	}
	return out
}

func (ord order) sortHits(hits []hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, _ := lookup(hits[i].value, ord.path)
		b, _ := lookup(hits[j].value, ord.path)
		c := compareAny(a, b, ord.desc)
		return c < 0
	})
}

// filter keeps the hits where prog is true. One VM serves the whole query.
// A row that throws counts as false; when all rows throw, the first error returns.
func filter(ctx context.Context, prog *goja.Program, hits []hit) ([]hit, error) {
	vm := goja.New()
	timer := time.AfterFunc(whereTimeout, func() { vm.Interrupt("timeout") })
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { vm.Interrupt("cancelled") })
	defer stop()
	out := make([]hit, 0, len(hits))
	var firstErr error
	throws := 0
	for _, h := range hits {
		var at any
		if h.at != "" {
			at = h.at
		}
		if err := errors.Join(vm.Set("v", h.value), vm.Set("k", h.key), vm.Set("at", at)); err != nil {
			return nil, fmt.Errorf("state where: %w", err)
		}
		v, err := vm.RunProgram(prog)
		if err != nil {
			var ie *goja.InterruptedError
			if errors.As(err, &ie) {
				return nil, fmt.Errorf("%w: where: stopped (%v); the limit is %s", ErrInvalid, ie.Value(), whereTimeout)
			}
			throws++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if v.ToBoolean() {
			out = append(out, h)
		}
	}
	if throws > 0 && throws == len(hits) {
		return nil, fmt.Errorf("%w: where: %v", ErrInvalid, firstErr)
	}
	return out, nil
}

func parseOrder(s string) (order, error) {
	ord := order{}
	field := s
	if strings.HasPrefix(field, "-") {
		ord.desc, field = true, field[1:]
	}
	dir := " ASC"
	if ord.desc {
		dir = " DESC"
	}
	switch field {
	case "", "key":
		ord.sql = "key" + dir
		return ord, nil
	case "at":
		ord.sql = "COALESCE(at, created_at)" + dir + ", key ASC"
		return ord, nil
	case "updated", "updated_at":
		ord.sql = "updated_at" + dir + ", key ASC"
		return ord, nil
	case "created", "created_at":
		ord.sql = "created_at" + dir + ", key ASC"
		return ord, nil
	}
	if path, ok := strings.CutPrefix(field, "v."); ok && path != "" {
		ord.path = strings.Split(path, ".")
		return ord, nil
	}
	return ord, fmt.Errorf("%w: sort %q: use at, updated, created, key, or v.<field>, with - for descending", ErrInvalid, s)
}

func parseCursor(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err == nil {
		if rest, ok := strings.CutPrefix(string(b), cursorPrefix); ok {
			if n, err := strconv.Atoi(rest); err == nil && n >= 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("%w: cursor %q is not valid", ErrInvalid, c)
}

// ftsQuery quotes each term, so text such as "foo-bar" is not FTS5 syntax.
// A term that ends in * is a prefix match.
func ftsQuery(q string) string {
	var terms []string
	for _, t := range strings.Fields(q) {
		star := ""
		if strings.HasSuffix(t, "*") {
			star, t = "*", strings.TrimRight(t, "*")
		}
		if t == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`+star)
	}
	return strings.Join(terms, " ")
}

func lookup(v any, path []string) (any, bool) {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[p]; !ok {
			return nil, false
		}
	}
	return v, true
}

// cut shortens each string in v to cutRunes characters.
func cut(v any) any {
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) <= cutRunes {
			return x
		}
		return string([]rune(x)[:cutRunes]) + "…"
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cut(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cut(e)
		}
		return out
	}
	return v
}

// compareAny orders numbers, then strings, then other values. A missing
// value is always last.
func compareAny(a, b any, desc bool) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	c := 0
	ra, rb := rank(a), rank(b)
	switch {
	case ra != rb:
		c = ra - rb
	case ra == 0:
		fa, fb := a.(float64), b.(float64)
		if fa < fb {
			c = -1
		} else if fa > fb {
			c = 1
		}
	case ra == 1:
		c = strings.Compare(a.(string), b.(string))
	}
	if desc {
		return -c
	}
	return c
}

func rank(v any) int {
	switch v.(type) {
	case float64:
		return 0
	case string:
		return 1
	case bool:
		return 2
	}
	return 3
}
