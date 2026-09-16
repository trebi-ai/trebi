package cli

import (
	"testing"
	"time"
)

func TestTrackerIdleOnlyAfterResult(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	if got := tr.Snapshot().State; got != StatusRunning {
		t.Fatalf("after prompt: %s", got)
	}
	snap := tr.Observe(Event{Type: EventAssistant, Text: "hi"})
	if snap.State != StatusRunning || !snap.Progressing {
		t.Fatalf("during turn: %+v", snap)
	}
	snap = tr.Observe(Event{Type: EventResult, Result: &Result{Subtype: "success"}})
	if snap.State != StatusIdle || snap.LastResult == nil {
		t.Fatalf("after result: %+v", snap)
	}
}

func TestTrackerInflightToolBlocksIdleAndStall(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{ID: "t1", Name: "Bash", Status: "started"}})
	snap := tr.Snapshot()
	if snap.State != StatusRunning || !snap.Progressing || snap.Stalled || snap.InflightTools != 1 {
		t.Fatalf("in-flight: %+v", snap)
	}
	tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{ID: "t1", Name: "Bash", Status: "completed"}})
	if snap := tr.Snapshot(); snap.InflightTools != 0 {
		t.Fatalf("completed tool still in flight: %+v", snap)
	}
}

func TestTrackerApprovalSuspendsAndResumes(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	tr.Observe(Event{Type: EventPermission, Permission: &Permission{ID: "p1"}})
	if got := tr.Snapshot().State; got != StatusWaiting {
		t.Fatalf("approval open: %s", got)
	}
	if id := tr.OldestApprovalID(); id != "p1" {
		t.Fatalf("oldest approval: %q", id)
	}
	tr.PermissionDecided("p1")
	// Turn is still active: back to running, not idle.
	if got := tr.Snapshot().State; got != StatusRunning {
		t.Fatalf("after decision: %s", got)
	}
	tr.Observe(Event{Type: EventResult, Result: &Result{Subtype: "success"}})
	if got := tr.Snapshot().State; got != StatusIdle {
		t.Fatalf("after result: %s", got)
	}
}

func TestTrackerStallOnlyWithoutEventsAndInflight(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	// Force the last event into the past beyond the stall threshold.
	tr.mu.Lock()
	tr.lastEventAt = time.Now().Add(-StallThreshold - time.Minute)
	tr.mu.Unlock()
	snap := tr.Snapshot()
	if !snap.Stalled {
		t.Fatalf("expected stall: %+v", snap)
	}
	// A fresh event clears it.
	tr.Observe(Event{Type: EventAssistant, Text: "progress"})
	if snap := tr.Snapshot(); snap.Stalled {
		t.Fatalf("stall did not clear: %+v", snap)
	}
}

func TestTrackerHookApprovalDoesNotStack(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	tr.Observe(Event{Type: EventPermission, Permission: &Permission{ID: "wire-1"}})
	tr.PermissionAdded("hook-1")
	if got := tr.Snapshot().OpenApprovals; got != 2 {
		t.Fatalf("open approvals: %d", got)
	}
	// Deciding the wire approval keeps the fallback hook one open.
	tr.PermissionDecided("wire-1")
	if got := tr.Snapshot().OpenApprovals; got != 1 {
		t.Fatalf("after wire decision: %d", got)
	}
	tr.PermissionDecided("")
	if got := tr.Snapshot().State; got != StatusRunning {
		t.Fatalf("all decided, turn active: %s", got)
	}
}
