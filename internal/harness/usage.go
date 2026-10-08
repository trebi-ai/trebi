package harness

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
)

// Usage sources (plan 2026-10-08 D12): a run frame, an app-server probe, or
// a Codex rollout file.
const (
	UsageSourceRun         = "run"
	UsageSourceProbe       = "probe"
	UsageSourceSessionFile = "session_file"
)

// ErrRefreshTooSoon is returned when a refresh comes less than a minute
// after the last one.
var ErrRefreshTooSoon = errors.New("limits were read less than a minute ago")

// HarnessUsage is the plan limit state of one harness.
type HarnessUsage struct {
	Harness string        `json:"harness"`
	Plan    string        `json:"plan,omitempty"`
	Windows []UsageWindow `json:"windows"`
}

// UsageWindow is one stored limit window. Percent is 0–100; times are
// RFC 3339 UTC.
type UsageWindow struct {
	Key         string  `json:"key"`
	Scope       string  `json:"scope,omitempty"`
	Minutes     int     `json:"minutes"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    string  `json:"resets_at,omitempty"`
	Source      string  `json:"source"`
	ObservedAt  string  `json:"observed_at"`
}

// UsageStore owns the harness_usage rows.
type UsageStore struct {
	st  *store.Store
	now func() time.Time
}

// NewUsageStore creates a usage store.
func NewUsageStore(st *store.Store) *UsageStore {
	return &UsageStore{st: st, now: time.Now}
}

// Record replaces the windows of one harness that the limits name. Other
// windows stay. A rejected limit sets harness_state.limit_exceeded; a later
// record with all windows below 100 % clears it.
func (s *UsageStore) Record(ctx context.Context, harness string, l agentwire.Limits, source string, at time.Time) error {
	if harness == "" || (len(l.Windows) == 0 && !l.Rejected) {
		return nil
	}
	if at.IsZero() {
		at = s.now()
	}
	observed := at.UTC().Format(time.RFC3339)
	below := len(l.Windows) > 0
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		for _, w := range l.Windows {
			var resets any
			if !w.ResetsAt.IsZero() {
				resets = w.ResetsAt.UTC().Format(time.RFC3339)
			}
			if w.UsedPercent >= 100 {
				below = false
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO harness_usage
				(harness, window_key, scope, minutes, used_percent, resets_at, plan, source, observed_at)
				VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT(harness, window_key, scope, minutes) DO UPDATE SET
					used_percent=excluded.used_percent, resets_at=excluded.resets_at, plan=excluded.plan,
					source=excluded.source, observed_at=excluded.observed_at`,
				harness, w.Key, w.Scope, w.Minutes, w.UsedPercent, resets, l.Plan, source, observed); err != nil {
				return err
			}
		}
		switch {
		case l.Rejected:
			_, err := tx.ExecContext(ctx, `INSERT INTO harness_state (name, limit_exceeded, updated_at) VALUES (?,1,?)
				ON CONFLICT(name) DO UPDATE SET limit_exceeded=1, updated_at=excluded.updated_at`, harness, observed)
			return err
		case below:
			_, err := tx.ExecContext(ctx, `UPDATE harness_state SET limit_exceeded=0, updated_at=? WHERE name=? AND limit_exceeded=1`, observed, harness)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = s.st.Append(ctx, store.BusEvent{
		Topic: "harness.usage_updated", Entity: "harness", EntityID: harness, Action: "updated",
		Attrs: map[string]string{"harness": harness, "source": source},
	})
	return err
}

// windowOrder is the display order of window keys.
var windowOrder = []string{agentwire.WindowFiveHour, agentwire.WindowDaily, agentwire.WindowWeekly, agentwire.WindowMonthly, agentwire.WindowCustom} //nolint:gochecknoglobals // static order, never mutated

// List returns the windows of each harness with rows. A window whose reset
// time is past shows 0 % and no reset time.
func (s *UsageStore) List(ctx context.Context) ([]HarnessUsage, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT harness, window_key, scope, minutes, used_percent,
		COALESCE(resets_at,''), plan, source, observed_at FROM harness_usage ORDER BY harness, observed_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now().UTC()
	out := []HarnessUsage{}
	for rows.Next() {
		var h, plan string
		var w UsageWindow
		if err := rows.Scan(&h, &w.Key, &w.Scope, &w.Minutes, &w.UsedPercent, &w.ResetsAt, &plan, &w.Source, &w.ObservedAt); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil && !t.After(now) {
			w.UsedPercent, w.ResetsAt = 0, ""
		}
		if len(out) == 0 || out[len(out)-1].Harness != h {
			out = append(out, HarnessUsage{Harness: h, Plan: plan})
		}
		out[len(out)-1].Windows = append(out[len(out)-1].Windows, w)
	}
	for i := range out {
		slices.SortStableFunc(out[i].Windows, func(a, b UsageWindow) int {
			if d := slices.Index(windowOrder, a.Key) - slices.Index(windowOrder, b.Key); d != 0 {
				return d
			}
			if d := a.Minutes - b.Minutes; d != 0 {
				return d
			}
			return strings.Compare(a.Scope, b.Scope)
		})
	}
	return out, rows.Err()
}

// lastObserved returns the newest observed_at of one harness.
func (s *UsageStore) lastObserved(ctx context.Context, harness string) time.Time {
	var v sql.NullString
	_ = s.st.DB().QueryRowContext(ctx, `SELECT MAX(observed_at) FROM harness_usage WHERE harness=?`, harness).Scan(&v) // no row is the zero time
	t, _ := time.Parse(time.RFC3339, v.String)
	return t
}

// LimitsReader reads the plan limits of a harness without a run.
// *agentwire.Runtime implements it.
type LimitsReader interface {
	Limits(ctx context.Context, h agentwire.Harness, q agentwire.ModelQuery) (agentwire.Limits, error)
}

// authStates is the auth view the poller reads. *Service implements it.
type authStates interface {
	States(ctx context.Context) (map[string]State, error)
}

// Poller intervals.
const (
	UsagePollEvery   = 15 * time.Minute
	usagePollBackoff = 60 * time.Minute
	usageRefreshGap  = time.Minute
)

// UsagePoller reads the Codex limits while Codex is logged in: from the
// app-server, else from the newest rollout file of a TUI run.
type UsagePoller struct {
	usage  *UsageStore
	reader LimitsReader
	auth   authStates
	every  time.Duration
	// sessionDirs are the Codex rollout roots; tests replace them.
	sessionDirs []string
	log         *slog.Logger

	mu       sync.Mutex
	lastRead time.Time
}

// NewUsagePoller creates the poller. every is the interval while the probe works.
func NewUsagePoller(usage *UsageStore, reader LimitsReader, auth authStates, every time.Duration) *UsagePoller {
	return &UsagePoller{
		usage: usage, reader: reader, auth: auth, every: every,
		sessionDirs: []string{filepath.Join(UserHome(), ".codex", "sessions"), filepath.Join(config.HarnessDir("codex"), "sessions")},
		log:         slog.Default().With("source", "harness_usage"),
	}
}

// WithSessionDirs replaces the Codex rollout roots.
func (p *UsagePoller) WithSessionDirs(dirs ...string) *UsagePoller {
	p.sessionDirs = dirs
	return p
}

// Store is the usage store the poller writes.
func (p *UsagePoller) Store() *UsageStore { return p.usage }

// Run polls once at start and then on the interval until ctx ends.
func (p *UsagePoller) Run(ctx context.Context) {
	for {
		wait := p.every
		if p.codexLoggedIn(ctx) {
			if err := p.poll(ctx); err != nil {
				p.log.Debug("codex limits probe", "error", err)
				wait = usagePollBackoff
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Refresh reads the Codex limits now. It refuses a second call within a minute.
func (p *UsagePoller) Refresh(ctx context.Context) error {
	p.mu.Lock()
	if !p.lastRead.IsZero() && p.usage.now().Sub(p.lastRead) < usageRefreshGap {
		p.mu.Unlock()
		return ErrRefreshTooSoon
	}
	p.lastRead = p.usage.now()
	p.mu.Unlock()
	return p.poll(ctx)
}

func (p *UsagePoller) codexLoggedIn(ctx context.Context) bool {
	if p.auth == nil {
		return false
	}
	states, err := p.auth.States(ctx)
	if err != nil {
		return false
	}
	st, ok := states["codex"]
	return ok && st.AuthOK.Valid && st.AuthOK.Int64 == 1
}

// poll probes the app-server and falls back to the rollout file. A newer
// rollout line also wins over the stored value when the probe fails.
func (p *UsagePoller) poll(ctx context.Context) error {
	var probeErr error
	if p.reader == nil {
		probeErr = errors.New("no limits reader")
	} else if l, err := p.reader.Limits(ctx, agentwire.Codex, agentwire.ModelQuery{}); err != nil {
		probeErr = err
	} else if len(l.Windows) > 0 || l.Rejected {
		return p.usage.Record(ctx, "codex", l, UsageSourceProbe, time.Time{})
	}
	l, at, ok := newestRolloutLimits(p.sessionDirs)
	if !ok || !at.After(p.usage.lastObserved(ctx, "codex")) {
		return probeErr
	}
	return p.usage.Record(ctx, "codex", l, UsageSourceSessionFile, at)
}

// newestRolloutLimits reads the last token_count rate_limits line of the
// newest Codex rollout file under the roots.
func newestRolloutLimits(roots []string) (agentwire.Limits, time.Time, bool) {
	var newest string
	var newestMod time.Time
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil //nolint:nilerr // a missing or unreadable root has no rollout
			}
			if info, err := d.Info(); err == nil && info.ModTime().After(newestMod) {
				newest, newestMod = path, info.ModTime()
			}
			return nil
		})
	}
	if newest == "" {
		return agentwire.Limits{}, time.Time{}, false
	}
	f, err := os.Open(newest)
	if err != nil {
		return agentwire.Limits{}, time.Time{}, false
	}
	defer f.Close()
	var last rolloutLine
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if !strings.Contains(string(b), `"rate_limits"`) {
			continue
		}
		var line rolloutLine
		if json.Unmarshal(b, &line) != nil || line.Type != "event_msg" || line.Payload.Type != "token_count" || line.Payload.RateLimits == nil {
			continue
		}
		last, found = line, true
	}
	if !found {
		return agentwire.Limits{}, time.Time{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, last.Timestamp)
	if at.IsZero() {
		at = newestMod
	}
	return last.Payload.RateLimits.limits(at), at.UTC(), true
}

// rolloutLine is one line of a Codex rollout file, reduced to the limits.
type rolloutLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type       string             `json:"type"`
		RateLimits *rolloutRateLimits `json:"rate_limits"`
	} `json:"payload"`
}

type rolloutRateLimits struct {
	LimitID   string         `json:"limit_id"`
	PlanType  string         `json:"plan_type"`
	Primary   *rolloutWindow `json:"primary"`
	Secondary *rolloutWindow `json:"secondary"`
}

type rolloutWindow struct {
	UsedPercent     float64 `json:"used_percent"`
	WindowMinutes   int     `json:"window_minutes"`
	ResetsAt        int64   `json:"resets_at"`
	ResetsInSeconds int64   `json:"resets_in_seconds"`
}

func (r rolloutRateLimits) limits(at time.Time) agentwire.Limits {
	l := agentwire.Limits{Plan: r.PlanType}
	scope := r.LimitID
	if scope == "codex" {
		scope = ""
	}
	for _, w := range []*rolloutWindow{r.Primary, r.Secondary} {
		if w == nil {
			continue
		}
		lw := agentwire.LimitWindow{Key: agentwire.WindowKey(w.WindowMinutes), Scope: scope, Minutes: w.WindowMinutes, UsedPercent: w.UsedPercent}
		switch {
		case w.ResetsAt > 0:
			lw.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
		case w.ResetsInSeconds > 0:
			lw.ResetsAt = at.Add(time.Duration(w.ResetsInSeconds) * time.Second).UTC()
		}
		l.Windows = append(l.Windows, lw)
	}
	return l
}
