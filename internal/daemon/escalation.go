package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// RouteFailure wakes the owning agent before a run_failed reaches the inbox.
// Three failures in 24 hours exhaust the hand-off; the fourth raises.
func (c *ControlPlane) RouteFailure(ctx context.Context, f alerts.RunFailure) (bool, error) {
	var n int
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	err := c.Store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM event_queue
WHERE connector='escalation' AND workspace_id=? AND created_at >= ? AND event_id LIKE ?`,
		f.WorkspaceID, cutoff, "escalation:"+f.Job+":%").Scan(&n)
	if err != nil {
		return false, err
	}
	if n >= 3 {
		return false, nil
	}
	ownerJob, _ := c.findJobKind(ctx, f.WorkspaceID, f.Owner, runs.KindAgent)
	if ownerJob == nil {
		return false, nil
	}
	if allowed, reason, err := c.Workspaces.Allow(ctx, f.WorkspaceID, f.Job, runs.KindJob); err != nil {
		return false, err
	} else if !allowed {
		c.log.Warn("agent escalation over budget", "agent", f.Owner, "reason", reason)
		return false, nil
	}
	eventID := "escalation:" + f.Job + ":" + f.RunID
	message := fmt.Sprintf("Owned job failed.\n\nJob: %s\nFailed run: %s\nEnd reason: %s\n",
		f.Job, f.RunID, f.EndReason)
	if tail := c.transcriptTail(ctx, f.RunID, 40); tail != "" {
		message += "\nTranscript tail:\n" + tail + "\n"
	}
	var duplicate int
	if err := c.Store.DB().QueryRowContext(ctx,
		`SELECT 1 FROM event_queue WHERE event_id=? LIMIT 1`, eventID).Scan(&duplicate); err == nil && duplicate == 1 {
		return true, nil
	}
	item := &dispatch.QueueItem{
		EventID:     eventID,
		Connector:   "escalation",
		WorkspaceID: f.WorkspaceID,
		Job:         f.Owner,
		Kind:        runs.KindAgent,
		Prompt:      message,
		Status:      dispatch.QueuePending,
	}
	if err := c.queue.Enqueue(ctx, item); err != nil {
		return false, nil
	}
	c.log.Info("agent.escalated", "agent", f.Owner, "to", "agent-owner", "run", f.RunID)
	return true, nil
}

// transcriptTail returns the last N text turns from the same sources as the
// transcript API, so the owner sees what happened before taking over.
func (c *ControlPlane) transcriptTail(ctx context.Context, runID string, n int) string {
	run, err := c.Runs.Get(ctx, runID)
	if err != nil || n <= 0 {
		return ""
	}
	var turns []tmux.Turn
	switch {
	case run.Runtime == "cli":
		turns, err = c.Runs.CLITurns(ctx, run.ID)
		if err != nil {
			return ""
		}
	default:
		tool, ok := tmux.ByName(run.Harness)
		if !ok || run.SessionID == "" {
			return ""
		}
		workingDir := tmux.TranscriptWorkingDir(run.Harness, run.WorkingDir, run.StartedAt)
		path, err := tool.TranscriptPath(home.HarnessDir(run.Harness), workingDir, run.SessionID)
		if err != nil {
			return ""
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return ""
		}
		turns, err = tool.ParseTranscript(bytes.NewReader(data))
		if err != nil {
			return ""
		}
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	var out []string
	for _, turn := range turns {
		if text := strings.TrimSpace(turn.Text); text != "" {
			out = append(out, turn.Role+": "+text)
		}
	}
	return strings.Join(out, "\n")
}
