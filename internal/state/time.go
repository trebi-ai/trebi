package state

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimeRange selects rows by time. A zero Since or Until is open.
// By is "at" (default), "updated", or "created".
type TimeRange struct {
	Since, Until time.Time
	By           string
}

// RunRef names one run and its start.
type RunRef struct {
	ID    string
	Start time.Time
}

// RunClock finds the last complete run of an owner, other than exceptRunID.
// The runs service supplies it.
type RunClock interface {
	LastComplete(ctx context.Context, ownerID, exceptRunID string) (RunRef, bool, error)
}

// span is a relative length such as "7d" or "3mo".
type span struct {
	n    int
	unit string
}

const tsLayout = "2006-01-02T15:04:05Z"

var spanRE = regexp.MustCompile(`^([0-9]{1,6})(h|d|w|mo|y)$`)

// ParseTime reads "2026-09-01", "2026-09-01 10:00", RFC 3339, "7d", "2w",
// "3mo", "1y", "12h", "today", "yesterday", or "last-run". Relative values
// count back from now. A date with no zone is in loc (nil = UTC).
func ParseTime(s string, now time.Time, loc *time.Location, lastRun func() (time.Time, bool)) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return time.Time{}, fmt.Errorf("%w: empty time", ErrInvalid)
	case "today", "yesterday":
		y, m, d := now.In(loc).Date()
		day := time.Date(y, m, d, 0, 0, 0, 0, loc)
		if s == "yesterday" {
			day = day.AddDate(0, 0, -1)
		}
		return day.UTC(), nil
	case "last-run":
		if lastRun == nil {
			return time.Time{}, fmt.Errorf("%w: last-run: no complete run before this one", ErrInvalid)
		}
		t, ok := lastRun()
		if !ok {
			return time.Time{}, fmt.Errorf("%w: last-run: no complete run before this one", ErrInvalid)
		}
		return t.UTC(), nil
	}
	if sp, ok := parseSpan(s); ok {
		return sp.back(now).UTC(), nil
	}
	t, err := parseAbsolute(s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: time %q: use YYYY-MM-DD, YYYY-MM-DD HH:MM, RFC 3339, 7d, 2w, 3mo, 1y, 12h, today, yesterday, or last-run", ErrInvalid, s)
	}
	return t, nil
}

// parseAbsolute reads the three absolute forms and returns UTC.
func parseAbsolute(s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, loc); err == nil {
		return t.UTC(), nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func parseSpan(s string) (span, bool) {
	m := spanRE.FindStringSubmatch(s)
	if m == nil {
		return span{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n == 0 {
		return span{}, false
	}
	return span{n: n, unit: m[2]}, true
}

func (sp span) back(t time.Time) time.Time { return sp.add(t, -sp.n) }

func (sp span) forward(t time.Time) time.Time { return sp.add(t, sp.n) }

func (sp span) add(t time.Time, n int) time.Time {
	switch sp.unit {
	case "h":
		return t.Add(time.Duration(n) * time.Hour)
	case "d":
		return t.AddDate(0, 0, n)
	case "w":
		return t.AddDate(0, 0, 7*n)
	case "mo":
		return t.AddDate(0, n, 0)
	default:
		return t.AddDate(n, 0, 0)
	}
}

// column returns the SQL time expression for r.By.
func (r TimeRange) column() (string, error) {
	switch r.By {
	case "", "at":
		return "COALESCE(at, created_at)", nil
	case "updated":
		return "updated_at", nil
	case "created":
		return "created_at", nil
	}
	return "", fmt.Errorf("%w: by %q: use at, updated, or created", ErrInvalid, r.By)
}

func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }
