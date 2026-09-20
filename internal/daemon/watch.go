package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

const fsnotifyDebounce = 200 * time.Millisecond

func (c *ControlPlane) watchWorkspaces(ctx context.Context) {
	// Periodic full rebuild + live fsnotify for index-relevant file changes (C8).
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	pending := map[string]struct{}{}
	watched := map[string]struct{}{}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		c.log.Warn("fsnotify unavailable; index rebuild on poll only", "error", err)
		w = nil
	} else {
		defer w.Close()
		c.syncWatchPaths(ctx, w, watched)
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
				c.syncWatchPaths(ctx, w, watched)
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
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					if _, seen := watched[ev.Name]; !seen && w != nil {
						if w.Add(ev.Name) == nil {
							watched[ev.Name] = struct{}{}
						}
					}
					continue
				}
			}
			// Only files discovery indexes or reads for composition (C8).
			if !indexRelevantFile(ev.Name) {
				continue
			}
			pending[ev.Name] = struct{}{}
			debounce.Reset(fsnotifyDebounce)
		case <-debounce.C:
			for path := range pending {
				c.rebuildPath(ctx, path)
			}
			clear(pending)
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

// indexRelevantFile reports whether a path can change the index or wake
// composition (C8): the three definition files, ledgers, workspace config.
func indexRelevantFile(path string) bool {
	switch strings.ToLower(filepath.Base(path)) {
	case "job.md", "trebi.md", "agent.md", "memory.md", "trebi.yaml":
		return true
	}
	return false
}

// syncWatchPaths watches workspace roots and known job/agent dirs from
// job_index (and their ancestors). New directories are added on Create.
func (c *ControlPlane) syncWatchPaths(ctx context.Context, w *fsnotify.Watcher, watched map[string]struct{}) {
	add := func(path string) {
		if path == "" {
			return
		}
		if _, ok := watched[path]; ok {
			return
		}
		if err := w.Add(path); err == nil {
			watched[path] = struct{}{}
		}
	}
	list, err := c.Workspaces.List(ctx)
	if err != nil {
		c.log.Warn("watch sync list", "error", err)
		return
	}
	for _, ws := range list {
		add(ws.Path)
		jobs, err := c.Workspaces.ListJobs(ctx, ws.ID)
		if err != nil {
			c.log.Debug("watch sync jobs", "workspace", ws.ID, "error", err)
			continue
		}
		for _, row := range jobs {
			dir := filepath.Dir(row.Path)
			for {
				add(dir)
				if dir == ws.Path || len(dir) < len(ws.Path) {
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
		}
	}
}

func (c *ControlPlane) rebuildAll(ctx context.Context) {
	list, _ := c.Workspaces.List(ctx)
	for i := range list {
		c.recomputeFromIndex(ctx, &list[i])
	}
}

func (c *ControlPlane) rebuildPath(ctx context.Context, path string) {
	list, _ := c.Workspaces.List(ctx)
	for i := range list {
		ws := &list[i]
		if path == ws.Path || strings.HasPrefix(path, ws.Path+string(filepath.Separator)) {
			base := strings.ToLower(filepath.Base(path))
			if base == "memory.md" {
				return
			}
			if base == "trebi.yaml" {
				c.recomputeFromIndex(ctx, ws)
			} else {
				j, err := c.Workspaces.RebuildIndexPath(ctx, ws, path)
				if err == nil && j != nil {
					_ = c.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{})
				}
			}
			c.log.Debug("fsnotify rebuild", "workspace", ws.ID, "path", path)
			return
		}
	}
}

func (c *ControlPlane) recomputeFromIndex(ctx context.Context, ws *workspaces.Workspace) {
	jobs, err := c.Workspaces.RebuildIndexJobs(ctx, ws)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j == nil || j.ParseError != "" {
			continue
		}
		_ = c.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{})
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
		c.replyIfExchangeEnded(ctx, run)
	}
}

// replyIfExchangeEnded mails the run's answer when its exchange ended
// (plan I.2): the session left running, or the run itself ended. It runs
// after ingestRun stored new turns, so one reply covers them all.
func (c *ControlPlane) replyIfExchangeEnded(ctx context.Context, run *runs.Run) {
	if run.ConversationID == "" || c.Email == nil || c.Opts.Shadow || !c.Email.Configured() {
		return
	}
	state := c.Runs.SessionState(run.ID)
	if !run.Status.IsTerminal() && state != runs.SessionWaiting && state != runs.SessionIdle {
		return
	}
	if err := c.Email.ReplyOnThread(ctx, run.ID, run.ConversationID); err != nil {
		c.log.Warn("email reply", "run_id", run.ID, "error", err)
	}
}

type transcriptCursor struct {
	path      string
	offset    int64
	size      int64
	mtime     int64
	turnCount int
}

func readTranscriptDelta(path string, cur transcriptCursor) (data []byte, base int64, from int, skip bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, 0, false, err
	}
	mtime := st.ModTime().UnixNano()
	if cur.path == path && cur.size == st.Size() && cur.mtime == mtime {
		return nil, 0, 0, true, nil
	}
	if cur.path == path && cur.offset > 0 && cur.offset <= st.Size() {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, 0, false, err
		}
		defer f.Close()
		if _, err := f.Seek(cur.offset, io.SeekStart); err != nil {
			return nil, 0, 0, false, err
		}
		data, err = io.ReadAll(f)
		if err != nil {
			return nil, 0, 0, false, err
		}
		return data, cur.offset, cur.turnCount, false, nil
	}
	data, err = os.ReadFile(path)
	return data, 0, 0, false, err
}

func (c *ControlPlane) ingestRun(ctx context.Context, run *runs.Run) {
	if run.ConversationID == "" || run.SessionID == "" {
		return
	}
	tool, ok := tmux.ByName(run.Harness)
	if !ok {
		return
	}
	workingDir := tmux.TranscriptWorkingDir(run.Harness, run.WorkingDir, run.StartedAt)
	path, err := tool.TranscriptPath(home.HarnessDir(run.Harness), workingDir, run.SessionID)
	if err != nil {
		return
	}
	c.turnMu.Lock()
	cur := c.turnCursor[run.ID]
	c.turnMu.Unlock()
	data, base, from, skip, err := readTranscriptDelta(path, cur)
	if err != nil || skip {
		return
	}
	turns, consumed, err := tmux.ParseTranscriptBytes(tool, data)
	if err != nil {
		return
	}
	incoming := make([]conversations.IncomingTurn, len(turns))
	for i, t := range turns {
		incoming[i] = conversations.IncomingTurn{Role: t.Role, Text: t.Text, At: t.At}
	}
	if _, err := c.Conversations.IngestAssistantTurnsAt(ctx, run.ID, run.ConversationID, from, incoming); err != nil {
		c.log.Warn("turn ingest", "run_id", run.ID, "error", err)
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	c.turnMu.Lock()
	if c.turnCursor == nil {
		c.turnCursor = map[string]transcriptCursor{}
	}
	c.turnCursor[run.ID] = transcriptCursor{
		path:      path,
		offset:    base + consumed,
		size:      st.Size(),
		mtime:     st.ModTime().UnixNano(),
		turnCount: from + len(turns),
	}
	c.turnMu.Unlock()
}
