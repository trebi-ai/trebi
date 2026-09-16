package cli

import (
	"sync"
	"time"
)

// StallThreshold is the "running but no events and no in-flight tools" window
// that raises agent.stall. It is observability only (G.3); max duration is
// the killer.
const StallThreshold = 5 * time.Minute

// Tracker is the per-run activity tracker (G.3). Wire events are the only
// input; the reaper reads snapshots instead of panes.
type Tracker struct {
	mu            sync.Mutex
	lastEventAt   time.Time
	turnActive    bool
	inflight      map[string]time.Time
	openApprovals map[string]time.Time
	lastResult    *Result
	idleSince     time.Time
	waitingSince  time.Time
	stallSince    time.Time
	startedAt     time.Time
	lastEventKind EventType
	lastToolName  string
}

// Snapshot is the derived state the runs layer and reaper consume.
type Snapshot struct {
	State         SessionStatus
	TurnActive    bool
	Progressing   bool
	Stalled       bool
	StallSince    time.Time
	InflightTools int
	OpenApprovals int
	LastEventAt   time.Time
	LastEventKind EventType
	LastToolName  string
	IdleSince     time.Time
	WaitingSince  time.Time
	LastResult    *Result
}

// NewTracker builds an empty tracker.
func NewTracker() *Tracker {
	now := deadlineNow()
	return &Tracker{
		lastEventAt:   now,
		inflight:      map[string]time.Time{},
		openApprovals: map[string]time.Time{},
		startedAt:     now,
	}
}

// PromptWritten marks the optimistic running state when a prompt is written:
// model latency is not idleness (G.3).
func (t *Tracker) PromptWritten() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turnActive = true
	t.lastEventAt = deadlineNow()
	t.lastResult = nil
	t.idleSince = time.Time{}
	t.stallSince = time.Time{}
}

// Observe folds one event into the tracker and returns the derived snapshot.
func (t *Tracker) Observe(e Event) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := deadlineNow()
	if e.At.IsZero() {
		e.At = now
	}
	t.lastEventAt = e.At
	t.lastEventKind = e.Type
	switch e.Type {
	case EventAssistant, EventUser, EventUsage, EventTool, EventStatus:
		// Progress: clears a stall window.
		t.stallSince = time.Time{}
	case EventInit:
		// Init alone is not turn progress.
	case EventPermission:
		if e.Permission != nil {
			t.openApprovals[e.Permission.ID] = now
			t.waitingSince = now
		} else {
			t.openApprovals[""] = now
			t.waitingSince = now
		}
	case EventResult:
		t.turnActive = false
		t.stallSince = time.Time{}
		if e.Result != nil {
			r := *e.Result
			t.lastResult = &r
		} else {
			t.lastResult = &Result{}
		}
		t.idleSince = now
	case EventError:
		if e.ExitCode != nil || e.EndReason != "" || e.Error != "" {
			// A classified failure ends the turn from the tracker's view.
			t.turnActive = false
		}
	}
	if e.Type == EventTool && e.Tool != nil {
		t.lastToolName = e.Tool.Name
		switch e.Tool.Status {
		case "started", "":
			t.inflight[e.Tool.ID] = now
		case "completed", "failed":
			delete(t.inflight, e.Tool.ID)
		}
	}
	return t.snapshotLocked(now)
}

// PermissionDecided clears an open approval. An empty id clears the oldest
// one (hook fallback approvals have no wire id).
func (t *Tracker) PermissionDecided(id string) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" {
		var oldest string
		var at time.Time
		for k, v := range t.openApprovals {
			if oldest == "" || v.Before(at) {
				oldest, at = k, v
			}
		}
		delete(t.openApprovals, oldest)
	} else {
		delete(t.openApprovals, id)
	}
	if len(t.openApprovals) == 0 {
		t.waitingSince = time.Time{}
	}
	return t.snapshotLocked(deadlineNow())
}

// PermissionAdded registers a hook-fallback approval the wire did not open.
func (t *Tracker) PermissionAdded(id string) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.openApprovals[id] = deadlineNow()
	t.waitingSince = deadlineNow()
	return t.snapshotLocked(deadlineNow())
}

// OldestApprovalID returns the oldest open wire permission id, or "" when the
// pending approval came from a hook.
func (t *Tracker) OldestApprovalID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var oldest string
	var at time.Time
	for k, v := range t.openApprovals {
		if k == "" {
			continue
		}
		if oldest == "" || v.Before(at) {
			oldest, at = k, v
		}
	}
	return oldest
}

// Snapshot returns the derived state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(deadlineNow())
}

// InflightCount reports tool invocations without a matching result. The
// tracker knows this; a pane only ever offered "esc to interrupt".
func (t *Tracker) InflightCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inflight)
}

func (t *Tracker) snapshotLocked(now time.Time) Snapshot {
	inWaiting := len(t.openApprovals) > 0
	state := StatusIdle
	switch {
	case inWaiting:
		state = StatusWaiting
	case t.turnActive:
		state = StatusRunning
	}
	snap := Snapshot{
		State:         state,
		TurnActive:    t.turnActive,
		InflightTools: len(t.inflight),
		OpenApprovals: len(t.openApprovals),
		LastEventAt:   t.lastEventAt,
		LastEventKind: t.lastEventKind,
		LastToolName:  t.lastToolName,
		IdleSince:     t.idleSince,
		WaitingSince:  t.waitingSince,
		LastResult:    t.lastResult,
	}
	if t.turnActive {
		gap := now.Sub(t.lastEventAt)
		if len(t.inflight) > 0 || gap < StallThreshold {
			snap.Progressing = true
			if len(t.inflight) > 0 {
				snap.StallSince = time.Time{}
				t.stallSince = time.Time{}
			}
		} else {
			if t.stallSince.IsZero() {
				t.stallSince = t.lastEventAt.Add(StallThreshold)
			}
			snap.Stalled = true
			snap.StallSince = t.stallSince
		}
	}
	if snap.State == StatusIdle && t.idleSince.IsZero() && !t.startedAt.IsZero() {
		snap.IdleSince = t.lastEventAt
	}
	return snap
}
