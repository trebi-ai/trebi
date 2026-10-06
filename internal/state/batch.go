package state

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// batch checks the rows of one Put and plans the writes.
type batch struct {
	owner  Owner
	coll   string
	mode   Mode
	decl   *Collection // nil when the collection is not declared
	limits Limits
	now    time.Time
}

// write is one planned row. old is nil for a new row.
type write struct {
	key   string
	value string
	at    sql.NullString
	old   *stored
}

const maxKeyBytes = 256

func newBatch(o Owner, coll string, mode Mode, limits Limits, now time.Time) *batch {
	b := &batch{owner: o, coll: coll, mode: mode, limits: limits, now: now}
	if c, ok := o.Collections[coll]; ok {
		b.decl = &c
	}
	return b
}

// size checks the batch limits.
func (b *batch) size(rows []Row) []RowError {
	var errs []RowError
	if len(rows) == 0 {
		errs = append(errs, RowError{Path: "batch", Msg: "has no rows"})
	}
	if len(rows) > b.limits.BatchRows {
		errs = append(errs, RowError{Path: "batch", Msg: fmt.Sprintf("has %d rows, limit %d", len(rows), b.limits.BatchRows)})
	}
	total := 0
	for _, r := range rows {
		total += len(r.Key) + len(r.At) + len(r.Value) + 32*len(r.Inc)
	}
	if total > b.limits.BatchBytes {
		errs = append(errs, RowError{Path: "batch", Msg: fmt.Sprintf("has %d bytes, limit %d", total, b.limits.BatchBytes)})
	}
	return errs
}

// plan checks each row against the existing rows and returns the writes.
func (b *batch) plan(rows []Row, existing map[string]stored) ([]write, []RowError) {
	var errs []RowError
	writes := make([]write, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for i, r := range rows {
		var old *stored
		if e, ok := existing[r.Key]; ok {
			old = &e
		}
		w, rerrs := b.row(i+1, r, old, seen)
		if len(rerrs) > 0 {
			errs = append(errs, rerrs...)
			continue
		}
		writes = append(writes, w)
	}
	return writes, errs
}

// row checks one row and builds its final value.
func (b *batch) row(line int, r Row, old *stored, seen map[string]bool) (write, []RowError) {
	var errs []RowError
	fail := func(path, msg string) { errs = append(errs, RowError{Line: line, Path: path, Msg: msg}) }
	if msg := keyProblem(r.Key); msg != "" {
		fail("key", msg)
	} else if seen[r.Key] {
		fail("key", "is in the batch more than one time")
	}
	seen[r.Key] = true
	live := old != nil && old.live(b.now)
	w := write{key: r.Key, old: old}
	if live {
		w.at = old.at
	}
	if r.At != "" {
		t, err := parseAbsolute(r.At, b.owner.Loc)
		if err != nil {
			fail("at", fmt.Sprintf("%q is not a real date: use YYYY-MM-DD, YYYY-MM-DD HH:MM, or RFC 3339", r.At))
		} else {
			w.at = sql.NullString{String: formatTS(t), Valid: true}
		}
	}
	if live && b.mode == ModeInsert {
		fail("key", "already exists; use mode upsert or merge")
	}
	if len(r.Inc) > 0 && b.mode != ModeMerge {
		fail("inc", "is only for mode merge")
	}
	v, path, msg := b.value(r, old, live)
	if msg != "" {
		fail(path, msg)
		return w, errs
	}
	w.value = v
	return w, errs
}

// value returns the canonical JSON of the final value, or a failed check.
func (b *batch) value(r Row, old *stored, live bool) (value, path, msg string) {
	base, msg := b.base(r, old, live)
	if msg != "" {
		return "", "value", msg
	}
	for field, n := range r.Inc {
		if msg := addNumber(base, field, n); msg != "" {
			return "", "inc." + field, msg
		}
	}
	out, err := json.Marshal(base)
	if err != nil {
		return "", "value", err.Error()
	}
	if len(out) > b.limits.RowBytes {
		return "", "value", fmt.Sprintf("is %d bytes, limit %d", len(out), b.limits.RowBytes)
	}
	if b.decl != nil {
		var plain any
		if err := json.Unmarshal(out, &plain); err != nil {
			return "", "value", err.Error()
		}
		if p, m, ok := b.decl.check(plain); !ok {
			return "", p, m
		}
	}
	return string(out), "", ""
}

// base decodes the value of r and, in merge mode, merges it into the old value.
func (b *batch) base(r Row, old *stored, live bool) (map[string]any, string) {
	var cur map[string]any
	if live && b.mode == ModeMerge {
		cur, _ = decodeObject([]byte(old.value))
	}
	if cur == nil {
		cur = map[string]any{}
	}
	if len(r.Value) == 0 {
		if b.mode != ModeMerge || len(r.Inc) == 0 {
			return nil, "is missing"
		}
		return cur, ""
	}
	patch, ok := decodeObject(r.Value)
	if !ok {
		return nil, "must be a JSON object"
	}
	if b.mode != ModeMerge {
		return patch, ""
	}
	mergePatch(cur, patch)
	return cur, ""
}

// expires returns expires_at from the TTL of the collection.
func (b *batch) expires() sql.NullString {
	if b.decl == nil || b.decl.TTL == "" {
		return sql.NullString{}
	}
	sp, ok := parseSpan(b.decl.TTL)
	if !ok {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTS(sp.forward(b.now)), Valid: true}
}

func keyProblem(k string) string {
	switch {
	case k == "":
		return "is empty"
	case len(k) > maxKeyBytes:
		return fmt.Sprintf("is %d bytes, limit %d", len(k), maxKeyBytes)
	case !utf8.ValidString(k):
		return "is not valid UTF-8"
	case strings.IndexFunc(k, unicode.IsControl) >= 0:
		return "has a control character"
	}
	return ""
}

// decodeObject decodes a JSON object and keeps numbers as json.Number.
func decodeObject(b []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok && m != nil
}

// mergePatch applies an RFC 7386 merge patch to target.
func mergePatch(target, patch map[string]any) {
	for k, pv := range patch {
		if pv == nil {
			delete(target, k)
			continue
		}
		pm, ok := pv.(map[string]any)
		if !ok {
			target[k] = pv
			continue
		}
		tm, ok := target[k].(map[string]any)
		if !ok {
			tm = map[string]any{}
		}
		mergePatch(tm, pm)
		target[k] = tm
	}
}

// addNumber adds n to the number at the dotted path. An absent field starts at 0.
func addNumber(m map[string]any, path string, n float64) string {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p]
		if !ok {
			child := map[string]any{}
			m[p] = child
			m = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Sprintf("field %q is not an object", p)
		}
		m = child
	}
	last := parts[len(parts)-1]
	var cur float64
	switch x := m[last].(type) {
	case nil:
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return "target is not a number"
		}
		cur = f
	case float64:
		cur = x
	default:
		return "target is not a number"
	}
	m[last] = cur + n
	return ""
}
