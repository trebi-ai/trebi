package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tmux"
)

func (c *ControlPlane) watchWorkspaces(ctx context.Context) {
	// Periodic full rebuild + live fsnotify for JOB.md/TREBI.md changes.
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		c.log.Warn("fsnotify unavailable; index rebuild on poll only", "error", err)
		w = nil
	} else {
		defer w.Close()
		c.syncWatchPaths(ctx, w)
	}
	for {
		// A select on a nil channel blocks, so a dead watcher degrades to poll-only.
		var events <-chan fsnotify.Event
		var werrs <-chan error
		if w != nil {
			events = w.Events
			werrs = w.Errors
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w != nil {
				c.syncWatchPaths(ctx, w)
			}
			c.rebuildAll(ctx)
		case ev, ok := <-events:
			if !ok {
				w = nil
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			// Discovery indexes markdown files only.
			if !strings.HasSuffix(filepath.Base(ev.Name), ".md") {
				continue
			}
			c.rebuildPath(ctx, ev.Name)
		case err, ok := <-werrs:
			if !ok {
				continue
			}
			if err != nil {
				c.log.Debug("fsnotify", "error", err)
			}
		}
	}
}

// syncWatchPaths watches every directory under each workspace —
// discovery walks the full tree, so the watch must too.
func (c *ControlPlane) syncWatchPaths(ctx context.Context, w *fsnotify.Watcher) {
	list, err := c.Workspaces.List(ctx)
	if err != nil {
		c.log.Warn("watch sync list", "error", err)
		return
	}
	for _, ws := range list {
		root := ws.Path
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || !d.IsDir() {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			_ = w.Add(path)
			return nil
		})
		if err != nil {
			c.log.Debug("watch sync walk", "workspace", ws.ID, "error", err)
		}
	}
}

func (c *ControlPlane) rebuildAll(ctx context.Context) {
	list, _ := c.Workspaces.List(ctx)
	for i := range list {
		_ = c.Workspaces.RebuildIndex(ctx, &list[i])
		jobs, _ := c.Workspaces.ListJobs(ctx, list[i].ID)
		for _, row := range jobs {
			if j, err := job.ParseFile(row.Path); err == nil && j != nil {
				_ = c.Schedule.RecomputeFromJob(ctx, list[i].ID, j, time.Time{})
			}
		}
	}
}

func (c *ControlPlane) rebuildPath(ctx context.Context, path string) {
	list, _ := c.Workspaces.List(ctx)
	for i := range list {
		ws := &list[i]
		if path == ws.Path || strings.HasPrefix(path, ws.Path+string(filepath.Separator)) {
			_ = c.Workspaces.RebuildIndex(ctx, ws)
			jobs, _ := c.Workspaces.ListJobs(ctx, ws.ID)
			for _, row := range jobs {
				if j, err := job.ParseFile(row.Path); err == nil && j != nil {
					_ = c.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{})
				}
			}
			c.log.Debug("fsnotify rebuild", "workspace", ws.ID, "path", path)
			return
		}
	}
}

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
