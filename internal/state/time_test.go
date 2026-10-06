package state

import (
	"errors"
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	lastRun := func() (time.Time, bool) { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), true }
	noRun := func() (time.Time, bool) { return time.Time{}, false }
	cases := []struct {
		in   string
		loc  *time.Location
		last func() (time.Time, bool)
		want string // "" = error
	}{
		{"2026-09-01", nil, nil, "2026-09-01T00:00:00Z"},
		{"2026-09-01", ny, nil, "2026-09-01T04:00:00Z"},
		{"2026-09-01 10:30", ny, nil, "2026-09-01T14:30:00Z"},
		{"2026-09-01T10:30:00+02:00", ny, nil, "2026-09-01T08:30:00Z"},
		{"7d", nil, nil, "2026-09-29T12:00:00Z"},
		{"2w", nil, nil, "2026-09-22T12:00:00Z"},
		{"3mo", nil, nil, "2026-07-06T12:00:00Z"},
		{"1y", nil, nil, "2025-10-06T12:00:00Z"},
		{"12h", nil, nil, "2026-10-06T00:00:00Z"},
		{"today", nil, nil, "2026-10-06T00:00:00Z"},
		{"today", ny, nil, "2026-10-06T04:00:00Z"},
		{"yesterday", nil, nil, "2026-10-05T00:00:00Z"},
		{"last-run", nil, lastRun, "2026-10-05T08:00:00Z"},
		{"last-run", nil, noRun, ""},
		{"last-run", nil, nil, ""},
		{"2026-02-30", nil, nil, ""},
		{"0d", nil, nil, ""},
		{"7x", nil, nil, ""},
		{"", nil, nil, ""},
	}
	for _, tc := range cases {
		got, err := ParseTime(tc.in, now, tc.loc, tc.last)
		if tc.want == "" {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%q: err = %v, want ErrInvalid", tc.in, err)
			}
			continue
		}
		if err != nil || formatTS(got) != tc.want {
			t.Errorf("%q in %v = %s, %v; want %s", tc.in, tc.loc, formatTS(got), err, tc.want)
		}
	}
}
