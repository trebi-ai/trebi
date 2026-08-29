package daemon

import (
	"context"
	"os"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tmux"
)

const turnWatchEndedWindow = 2 * time.Minute

func (c *ControlPlane) turnWatch(ctx context.Context) {
	iv := c.Opts.TurnWatchInterval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	c.turnWatchTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.turnWatchTick(ctx)
		}
	}
}

func (c *ControlPlane) turnWatchTick(ctx context.Context) {
	if c.Conversations == nil {
		return
	}
	list, err := c.Runs.ListForTurnWatch(ctx, time.Now().UTC().Add(-turnWatchEndedWindow))
	if err != nil {
		c.log.Warn("turn watch list", "error", err)
		return
	}
	for _, run := range list {
		c.ingestRun(ctx, run)
	}
}

func (c *ControlPlane) ingestRun(ctx context.Context, run *runs.Run) {
	if run.ConversationID == "" || run.SessionID == "" {
		return
	}
	tool, ok := tmux.ByName(run.Harness)
	if !ok {
		return
	}
	path, err := tool.TranscriptPath(home.HarnessDir(run.Harness), "", run.SessionID)
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	turns, _, err := tmux.ParseTranscriptBytes(tool, data)
	if err != nil {
		return
	}
	incoming := make([]conversations.IncomingTurn, len(turns))
	for i, t := range turns {
		incoming[i] = conversations.IncomingTurn{Role: t.Role, Text: t.Text, At: t.At}
	}
	if _, err := c.Conversations.IngestAssistantTurns(ctx, run.ID, run.ConversationID, incoming); err != nil {
		c.log.Warn("turn ingest", "run_id", run.ID, "error", err)
	}
}
