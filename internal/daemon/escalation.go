package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// maxHandoffs is the number of hand-offs with no complete run between them
// after which a failure goes to the user.
const maxHandoffs = 2

// RouteFailure wakes the owning agent before a run_failed reaches the inbox.
// It counts the hand-offs since the last complete run of the job, so a
// daily job reaches the limit too. A hand-off that the agent marks done
// while the job still fails counts. After maxHandoffs the failure raises.
func (c *ControlPlane) RouteFailure(ctx context.Context, f alerts.RunFailure) (bool, error) {
	db := c.Store.DB()
	var since string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(COALESCE(ended_at, started_at)),'') FROM runs
WHERE job_id=? AND (CASE WHEN COALESCE(outcome,'') <> '' THEN outcome ELSE status END)='complete'`,
		f.JobID).Scan(&since); err != nil {
		return false, err
	}
	var n int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM event_queue
WHERE connector='escalation' AND workspace_id=? AND created_at > ? AND event_id LIKE ?`,
		f.WorkspaceID, since, "escalation:"+f.JobID+":%").Scan(&n)
	if err != nil {
		return false, err
	}
	if n >= maxHandoffs {
		return false, nil
	}
	ownerJob, _ := c.findJobKind(ctx, f.WorkspaceID, f.OwnerID, runs.KindAgent)
	if ownerJob == nil {
		return false, nil
	}
	if allowed, reason, err := c.Workspaces.Allow(ctx, f.WorkspaceID, f.JobName, runs.KindJob); err != nil {
		return false, err
	} else if !allowed {
		c.log.Warn("agent escalation over budget", "agent_id", f.OwnerID, "reason", reason)
		return false, nil
	}
	eventID := "escalation:" + f.JobID + ":" + f.RunID
	message := fmt.Sprintf("Owned job failed.\n\nJob: %s\nFailed run: %s\nEnd reason: %s\n",
		f.JobName, f.RunID, f.EndReason)
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
		JobID:       f.OwnerID,
		JobName:     ownerJob.Name,
		Kind:        runs.KindAgent,
		Prompt:      message,
		Status:      dispatch.QueuePending,
	}
	if err := c.queue.Enqueue(ctx, item); err != nil {
		c.log.Warn("escalation enqueue", "agent_id", f.OwnerID, "run", f.RunID, "error", err)
		return false, nil
	}
	c.log.Info("agent.escalated", "agent_id", f.OwnerID, "to", "agent-owner", "run", f.RunID)
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
