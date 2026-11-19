package connectors

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

// Receive modes of a connection: the events that Trebi keeps.
const (
	ReceiveAll  = "all"
	ReceiveSome = "some"
	ReceiveNone = "none"
)

// countsKeep is how long the hour buckets of connection_event_counts stay.
const countsKeep = 7 * 24 * time.Hour

// EventFilter selects the events that a connection sends to Trebi. A nil
// filter keeps all. The daemon applies it after the adapter: a skipped
// event moves the cursor, is counted, and is not stored.
type EventFilter struct {
	Mode  string   `yaml:"mode" json:"mode"`                       // all | some | none
	Types []string `yaml:"types,omitempty" json:"types,omitempty"` // with some: the kept types of the manifest
	Other bool     `yaml:"other,omitempty" json:"other,omitempty"` // with some: keep the types that the manifest does not list
}

// Keeps reports whether an event of type typ is stored. declared is the
// manifest events.types.
func (f *EventFilter) Keeps(typ string, declared []string) bool {
	if f == nil {
		return true
	}
	switch f.Mode {
	case ReceiveNone:
		return false
	case ReceiveSome:
		if slices.Contains(declared, typ) {
			return slices.Contains(f.Types, typ)
		}
		return f.Other
	}
	return true
}

// Validate checks the mode, and that each type is in declared.
func (f EventFilter) Validate(declared []string) []FieldError {
	var errs []FieldError
	switch f.Mode {
	case ReceiveAll, ReceiveNone:
		if len(f.Types) > 0 || f.Other {
			errs = append(errs, FieldError{Field: "receive.types", Message: "types and other go with mode some"})
		}
	case ReceiveSome:
		for i, t := range f.Types {
			switch {
			case !slices.Contains(declared, t):
				errs = append(errs, FieldError{Field: "receive.types", Message: "unknown event type " + strconv.Quote(t)})
			case slices.Index(f.Types, t) < i:
				errs = append(errs, FieldError{Field: "receive.types", Message: "lists " + strconv.Quote(t) + " twice"})
			}
		}
	default:
		errs = append(errs, FieldError{Field: "receive.mode", Message: "must be all, some, or none"})
	}
	return errs
}

// Prune removes the types that declared no longer lists, after a version
// update. A new type stays off under some.
func (f *EventFilter) Prune(declared []string) {
	if f == nil {
		return
	}
	f.Types = slices.DeleteFunc(f.Types, func(t string) bool { return !slices.Contains(declared, t) })
}

// equal reports whether two filters keep the same events. nil is all.
func (f *EventFilter) equal(o *EventFilter) bool {
	a, b := f.orAll(), o.orAll()
	return a.Mode == b.Mode && a.Other == b.Other && slices.Equal(a.Types, b.Types)
}

// orAll returns the filter, or mode all for nil.
func (f *EventFilter) orAll() EventFilter {
	if f == nil {
		return EventFilter{Mode: ReceiveAll}
	}
	return *f
}

// declaredTypes lists the manifest events.types.
func declaredTypes(m manifest.Manifest) []string {
	if m.Events == nil {
		return nil
	}
	out := make([]string, len(m.Events.Types))
	for i, t := range m.Events.Types {
		out[i] = t.Type
	}
	return out
}

// checkReceive validates a new filter against the manifest, and prunes an
// unchanged one. old is nil for a new connection. A mode all filter is
// stored as nil.
func (c *Connection) checkReceive(old *Connection, m manifest.Manifest) error {
	declared := declaredTypes(m)
	if old != nil && old.Receive.equal(c.Receive) {
		c.Receive.Prune(declared)
	} else if c.Receive != nil {
		if m.Events == nil && c.Receive.Mode != ReceiveAll {
			return &ValidationError{Name: c.ref(), Fields: []FieldError{{Field: "receive", Message: "the connector sends no events"}}}
		}
		if errs := c.Receive.Validate(declared); len(errs) > 0 {
			return &ValidationError{Name: c.ref(), Fields: errs}
		}
	}
	if c.Receive != nil && c.Receive.Mode == ReceiveAll {
		c.Receive = nil
	}
	return nil
}

// receiveOf returns a copy of the filter of a connection, or nil.
func (s *Service) receiveOf(id string) *EventFilter {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.conns[id].Receive
	if f == nil {
		return nil
	}
	cp := *f
	cp.Types = slices.Clone(f.Types)
	return &cp
}

// execer runs one statement; *sql.DB and *sql.Tx have it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// countType adds one event to the hour bucket of its connection and type.
// at is the RFC3339 receive time.
func countType(ctx context.Context, db execer, instance, typ, at string, skipped bool) error {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t = time.Now().UTC()
	}
	kept, skip := 1, 0
	if skipped {
		kept, skip = 0, 1
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO connection_event_counts (instance, type, hour, kept, skipped)
		VALUES (?,?,?,?,?)
		ON CONFLICT (instance, type, hour) DO UPDATE SET kept=connection_event_counts.kept+excluded.kept,
			skipped=connection_event_counts.skipped+excluded.skipped`,
		instance, typ, t.UTC().Truncate(time.Hour).Format(time.RFC3339), kept, skip); err != nil {
		return fmt.Errorf("count %s event of %s: %w", typ, instance, err)
	}
	return nil
}

// skipEvent handles an event that the filter of its connection skips: the
// cursor moves, so a change back to all does not replay it, and the skip
// is counted.
func (s *Service) skipEvent(ctx context.Context, rec eventRecord) error {
	if err := advanceConnectorCursor(rec.instance, rec.sourceID); err != nil {
		return err
	}
	return countType(ctx, s.st.DB(), rec.instance, rec.eventType, time.Now().UTC().Format(time.RFC3339Nano), true)
}

// PruneEventCounts deletes the hour buckets older than seven days.
func (s *Service) PruneEventCounts(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.st.DB().ExecContext(ctx, `DELETE FROM connection_event_counts WHERE hour < ?`,
		now.UTC().Add(-countsKeep).Truncate(time.Hour).Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("prune event counts: %w", err)
	}
	return res.RowsAffected()
}

// typeCount is the count of one event type in the last hour.
type typeCount struct{ kept, skipped int64 }

// typeCounts reads the counts of the last hour, by connection and type. The
// hour before the current bucket counts for the part of it that is still
// inside the last hour.
func (s *Service) typeCounts(now time.Time) map[string]map[string]typeCount {
	out := map[string]map[string]typeCount{}
	cur := now.UTC().Truncate(time.Hour)
	prev := cur.Add(-time.Hour)
	share := 1 - float64(now.Sub(cur))/float64(time.Hour)
	rows, err := s.st.DB().Query(`SELECT instance, type, hour, kept, skipped FROM connection_event_counts WHERE hour >= ?`,
		prev.Format(time.RFC3339))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var instance, typ, hour string
		var kept, skipped int64
		if rows.Scan(&instance, &typ, &hour, &kept, &skipped) != nil {
			continue
		}
		if hour == prev.Format(time.RFC3339) {
			kept = int64(math.Round(float64(kept) * share))
			skipped = int64(math.Round(float64(skipped) * share))
		}
		if out[instance] == nil {
			out[instance] = map[string]typeCount{}
		}
		c := out[instance][typ]
		out[instance][typ] = typeCount{kept: c.kept + kept, skipped: c.skipped + skipped}
	}
	return out
}

// eventCountsRow is the event_counts field of a connection row: one entry
// for each declared type, in manifest order, then each other type that came
// in the last hour.
func eventCountsRow(m manifest.Manifest, counts map[string]typeCount) []map[string]any {
	out := []map[string]any{}
	declared := declaredTypes(m)
	if m.Events != nil {
		for _, t := range m.Events.Types {
			c := counts[t.Type]
			out = append(out, map[string]any{"type": t.Type, "description": t.Description, "declared": true, "kept": c.kept, "skipped": c.skipped})
		}
	}
	var other []string
	for t, c := range counts {
		if !slices.Contains(declared, t) && c.kept+c.skipped > 0 {
			other = append(other, t)
		}
	}
	slices.Sort(other)
	for _, t := range other {
		c := counts[t]
		out = append(out, map[string]any{"type": t, "description": "", "declared": false, "kept": c.kept, "skipped": c.skipped})
	}
	return out
}

// fillReceive adds the receive, event_counts, and event_jobs fields to the
// row of a connection that sends events.
func fillReceive(row map[string]any, c Connection, m manifest.Manifest, counts map[string]typeCount, trigs []cachedTrig) {
	if m.Events == nil {
		return
	}
	row["receive"] = c.Receive.orAll()
	row["event_counts"] = eventCountsRow(m, counts)
	row["event_jobs"] = eventJobsRow(trigs, declaredTypes(m))
}

// namedTypes lists the declared types that a match expression names in
// quotes, such as event.type === 'screen.stable'. A trigger that names none
// starts on every type.
func namedTypes(match string, declared []string) []string {
	var out []string
	for _, t := range declared {
		if strings.Contains(match, `"`+t+`"`) || strings.Contains(match, `'`+t+`'`) || strings.Contains(match, "`"+t+"`") {
			out = append(out, t)
		}
	}
	return out
}

// eventJobsRow is the event_jobs field of a connection row: one entry for
// each live trigger and each type that it names. type is "" for a trigger
// that starts on every type.
func eventJobsRow(trigs []cachedTrig, declared []string) []map[string]any {
	out := []map[string]any{}
	for _, t := range trigs {
		if !t.live() || t.row.ID == "" {
			continue
		}
		types := namedTypes(t.tr.Match, declared)
		if len(types) == 0 {
			types = []string{""}
		}
		for _, typ := range types {
			out = append(out, map[string]any{"workspace": t.row.WorkspaceID, "job": t.row.Name, "type": typ})
		}
	}
	return out
}

// skippedBy reports the types of a trigger that a filter skips. A trigger
// that names no type is blocked only when the filter keeps nothing.
func skippedBy(f *EventFilter, match string, declared []string) []string {
	if f == nil {
		return nil
	}
	types := namedTypes(match, declared)
	if len(types) == 0 {
		if f.Mode == ReceiveNone || (f.Mode == ReceiveSome && len(f.Types) == 0 && !f.Other) {
			return []string{""}
		}
		return nil
	}
	var out []string
	for _, t := range types {
		if !f.Keeps(t, declared) {
			out = append(out, t)
		}
	}
	return out
}
