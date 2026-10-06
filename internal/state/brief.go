package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type CollectionStats struct {
	Name        string   `json:"name"`
	Rows        int      `json:"rows"`
	Bytes       int64    `json:"bytes"`
	NewestAt    string   `json:"newest_at,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	LastRun     *RunStat `json:"last_run,omitempty"`
	Fields      []string `json:"fields"` // schema properties, else the top 8 keys of the newest 50 rows
	Description string   `json:"description,omitempty"`
	TTL         string   `json:"ttl,omitempty"`
	Declared    bool     `json:"declared"`
}

// RunStat is the change count of one run in one collection.
type RunStat struct {
	RunID    string `json:"run_id"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Deleted  int    `json:"deleted"`
}

const (
	briefMax     = 20
	sampleRows   = 50
	sampleFields = 8
	descRunes    = 200
)

// Brief renders the "## State" prompt block for o. It returns "" when the
// owner declares no collections and has no rows.
func (s *Store) Brief(ctx context.Context, o Owner) (string, error) {
	stats, err := s.Collections(ctx, o)
	if err != nil || len(stats) == 0 {
		return "", err
	}
	noun := o.noun()
	loc := o.Loc
	if loc == nil {
		loc = time.UTC
	}
	table := [][]string{{"collection", "rows", "newest at", "last run", "fields", "description"}}
	for i, c := range stats {
		if i == briefMax {
			break
		}
		table = append(table, []string{
			c.Name, fmt.Sprint(c.Rows), dateIn(c.NewestAt, loc), c.LastRun.String(),
			strings.Join(c.Fields, ", "), oneLine(c.Description),
		})
	}
	var b strings.Builder
	b.WriteString("## State\n")
	fmt.Fprintf(&b, "This %s keeps its records in trebi state. Query state before you decide. Do not guess from memory.\n\n", noun)
	writeTable(&b, table)
	if n := len(stats) - briefMax; n > 0 {
		fmt.Fprintf(&b, "(+%d more: trebi state)\n", n)
	}
	fmt.Fprintf(&b, "\nCommands (the %s comes from this run):\n", noun)
	b.WriteString(`  trebi state query <collection> [--q text] [--where 'v.score >= 2'] [--since 7d|last-run] [--fields a,b] [--limit 50]
  trebi state has <collection> <key>...
  trebi state put <collection> --file rows.jsonl [--mode upsert|insert|merge]
  trebi state delete <collection> <key>...   trebi state prune <collection> --until 180d
MCP: tool "state" with actions summary, query, get, has, put, delete, prune, drop. Owner "self".
`)
	return b.String(), nil
}

// Collections returns the stats of each declared collection and each
// collection with live rows, sorted by name.
func (s *Store) Collections(ctx context.Context, o Owner) ([]CollectionStats, error) {
	byName, err := s.rowStats(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	for name, c := range o.Collections {
		st, ok := byName[name]
		if !ok {
			st = &CollectionStats{Name: name}
			byName[name] = st
		}
		st.Declared, st.Description, st.TTL = true, c.Description, c.TTL
		if c.Schema != nil {
			st.Fields = c.properties()
		}
	}
	if err := s.addLastRun(ctx, o, byName); err != nil {
		return nil, err
	}
	out := make([]CollectionStats, 0, len(byName))
	for _, st := range byName {
		if st.Fields == nil {
			if st.Fields, err = s.sampleFields(ctx, o.ID, st.Name); err != nil {
				return nil, err
			}
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) rowStats(ctx context.Context, owner string) (map[string]*CollectionStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT collection, COUNT(*),
COALESCE(SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB))), 0),
COALESCE(MAX(at), ''), MAX(updated_at)
FROM state_rows WHERE owner_id = ? AND `+liveSQL+` GROUP BY collection`, owner, formatTS(s.now()))
	if err != nil {
		return nil, fmt.Errorf("state stats: %w", err)
	}
	out := map[string]*CollectionStats{}
	for rows.Next() {
		var st CollectionStats
		if err := rows.Scan(&st.Name, &st.Rows, &st.Bytes, &st.NewestAt, &st.UpdatedAt); err != nil {
			_ = rows.Close() // the scan error wins
			return nil, fmt.Errorf("state stats: %w", err)
		}
		out[st.Name] = &st
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("state stats: %w", err)
	}
	return out, nil
}

// addLastRun sets LastRun from state_run_stats of the last complete run.
func (s *Store) addLastRun(ctx context.Context, o Owner, byName map[string]*CollectionStats) error {
	if s.clock == nil {
		return nil
	}
	ref, ok, err := s.clock.LastComplete(ctx, o.ID, o.RunID)
	if err != nil {
		return fmt.Errorf("state last run: %w", err)
	}
	if !ok {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT collection, inserted, updated, deleted
FROM state_run_stats WHERE owner_id = ? AND run_id = ?`, o.ID, ref.ID)
	if err != nil {
		return fmt.Errorf("state run stats: %w", err)
	}
	for rows.Next() {
		var name string
		rs := RunStat{RunID: ref.ID}
		if err := rows.Scan(&name, &rs.Inserted, &rs.Updated, &rs.Deleted); err != nil {
			_ = rows.Close() // the scan error wins
			return fmt.Errorf("state run stats: %w", err)
		}
		if st, ok := byName[name]; ok {
			st.LastRun = &rs
		}
	}
	return closeRows(rows)
}

// sampleFields returns the most common top-level keys of the newest rows.
func (s *Store) sampleFields(ctx context.Context, owner, coll string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT value FROM state_rows WHERE owner_id = ? AND collection = ?
ORDER BY updated_at DESC LIMIT ?`, owner, coll, sampleRows)
	if err != nil {
		return nil, fmt.Errorf("state fields: %w", err)
	}
	count := map[string]int{}
	for rows.Next() {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close() // the scan error wins
			return nil, fmt.Errorf("state fields: %w", err)
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			for k := range m {
				count[k]++
			}
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("state fields: %w", err)
	}
	out := make([]string, 0, len(count))
	for k := range count {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if count[out[i]] != count[out[j]] {
			return count[out[i]] > count[out[j]]
		}
		return out[i] < out[j]
	})
	return out[:min(len(out), sampleFields)], nil
}

// String gives the brief cell: "+11 ~2 -3", or "-" when nothing changed.
func (r *RunStat) String() string {
	if r == nil {
		return "-"
	}
	var parts []string
	if r.Inserted > 0 {
		parts = append(parts, fmt.Sprintf("+%d", r.Inserted))
	}
	if r.Updated > 0 {
		parts = append(parts, fmt.Sprintf("~%d", r.Updated))
	}
	if r.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("-%d", r.Deleted))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// noun names the owner kind from the ID prefix.
func (o Owner) noun() string {
	switch {
	case strings.HasPrefix(o.ID, "agt_"):
		return "agent"
	case strings.HasPrefix(o.ID, "ws_"):
		return "workspace"
	}
	return "job"
}

func dateIn(ts string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "-"
	}
	return t.In(loc).Format("2006-01-02")
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > descRunes {
		s = string([]rune(s)[:descRunes]) + "…"
	}
	return s
}

// writeTable writes a markdown table with padded columns.
func writeTable(b *strings.Builder, rows [][]string) {
	width := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			width[i] = max(width[i], utf8.RuneCountInString(c))
		}
	}
	line := func(cells []string) {
		b.WriteString("|")
		for i, c := range cells {
			b.WriteString(" " + c + strings.Repeat(" ", width[i]-utf8.RuneCountInString(c)) + " |")
		}
		b.WriteString("\n")
	}
	line(rows[0])
	b.WriteString("|")
	for _, w := range width {
		b.WriteString(strings.Repeat("-", w+2) + "|")
	}
	b.WriteString("\n")
	for _, r := range rows[1:] {
		line(r)
	}
}
