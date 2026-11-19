package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
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
	pendingDirs := map[string]struct{}{}
	pendingSkills := map[skills.Ref]struct{}{}
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
			if ref, ok := c.Skills.RefFor(ctx, ev.Name); ok {
				pendingSkills[ref] = struct{}{}
				debounce.Reset(fsnotifyDebounce)
			}
			// A moved or removed folder names no definition file, so it
			// rebuilds its whole workspace.
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					if _, seen := watched[ev.Name]; !seen && w != nil {
						if w.Add(ev.Name) == nil {
							watched[ev.Name] = struct{}{}
						}
					}
					pendingDirs[ev.Name] = struct{}{}
					debounce.Reset(fsnotifyDebounce)
					continue
				}
			}
			if _, isDir := watched[ev.Name]; isDir && ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				delete(watched, ev.Name)
				pendingDirs[ev.Name] = struct{}{}
				debounce.Reset(fsnotifyDebounce)
				continue
			}
			// Only files discovery indexes or reads for composition (C8).
			if !indexRelevantFile(ev.Name) {
				continue
			}
			pending[ev.Name] = struct{}{}
			debounce.Reset(fsnotifyDebounce)
		case <-debounce.C:
			c.rebuildDirs(ctx, pendingDirs)
			for path := range pending {
				c.rebuildPath(ctx, path)
			}
			for ref := range pendingSkills {
				if err := c.Skills.Reload(ctx, ref); err != nil {
					c.log.Debug("skill reload", "skill", ref.String(), "error", err)
				}
			}
			clear(pending)
			clear(pendingDirs)
			clear(pendingSkills)
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
	if isRoutesFile(path) {
		return true
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "job.md", "agent.md", "memory.md", "trebi.yaml":
		return true
	}
	return false
}

// isRoutesFile reports whether path is <workspace>/routes/<connection>.yaml.
func isRoutesFile(path string) bool {
	return filepath.Ext(path) == ".yaml" && filepath.Base(filepath.Dir(path)) == routing.Dir
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
		add(filepath.Join(ws.Path, routing.Dir))
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
	for _, dir := range c.Skills.WatchDirs(ctx) {
		add(dir)
	}
}

func (c *ControlPlane) rebuildAll(ctx context.Context) {
	list, _ := c.Workspaces.List(ctx)
	for i := range list {
		c.recomputeFromIndex(ctx, &list[i])
	}
}

// rebuildDirs rebuilds each workspace that holds one of dirs, once.
func (c *ControlPlane) rebuildDirs(ctx context.Context, dirs map[string]struct{}) {
	if len(dirs) == 0 {
		return
	}
	list, _ := c.Workspaces.List(ctx)
	for i := range list {
		ws := &list[i]
		for dir := range dirs {
			if strings.HasPrefix(dir, ws.Path+string(filepath.Separator)) {
				c.recomputeFromIndex(ctx, ws)
				break
			}
		}
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
			if isRoutesFile(path) {
				// The trigger index fingerprints the routes files, so it reloads itself.
				c.Workspaces.RoutesFileChanged(ctx, ws, path)
				return
			}
			if base == "trebi.yaml" {
				c.recomputeFromIndex(ctx, ws)
				c.emitAssistantSettings(ctx, ws)
			} else {
				j, err := c.Workspaces.RebuildIndexPath(ctx, ws, path)
				if err == nil && j != nil {
					_ = c.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{})
				}
				// An archive of an agent also stops the schedules of its jobs.
				if base == "agent.md" {
					c.recomputeFromIndex(ctx, ws)
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
	// A restart paused the run: the resume after boot ends the exchange.
	if run.ConversationID == "" || (run.Status == runs.StatusParked && run.EndReason == runs.EndRestart) {
		return
	}
	state := c.Runs.SessionState(run.ID)
	if !run.Status.IsTerminal() && state != runs.SessionWaiting && state != runs.SessionIdle {
		return
	}
	if c.Channels != nil && !c.Opts.Shadow {
		if err := c.Channels.SendReplies(ctx, run.ConversationID, run.ID); err != nil {
			c.log.Warn("channel reply", "run_id", run.ID, "error", err)
		}
	}
	if c.Email == nil || c.Opts.Shadow || !c.Email.Configured() {
		return
	}
	opts := email.ReplyOpts{RunID: run.ID, ConversationID: run.ConversationID}
	if conv, err := c.Conversations.Get(ctx, run.ConversationID); err == nil && conv.OperatorID != "" {
		short := conv.OperatorName
		opts.FromName = short
		if j, _ := c.findJobKind(ctx, run.Workspace, conv.OperatorID, runs.KindAgent); j != nil && j.Title != "" {
			opts.FromName = j.Title
		}
		if short != "" {
			opts.Agent = short
		}
	}
	if err := c.Email.ReplyOnThread(ctx, opts); err != nil {
		c.log.Warn("email reply", "run_id", run.ID, "error", err)
	}
	c.sendReportIfDue(ctx, run)
}

type transcriptCursor struct {
	path      string
	offset    int64
	size      int64
	mtime     int64
	turnCount int
	memoryFix bool // the last user turn is a memory fix
	// unanswered: the last user prompt has no model turn yet.
	unanswered bool
	// fixIsReply: the current memory fix turn holds the reply to that prompt.
	fixIsReply bool
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

// IngestTurns stores the new turns of one run now. Reply capture calls it at
// turn end, so it does not wait for the next watch tick.
func (c *ControlPlane) IngestTurns(ctx context.Context, runID string) {
	if run, err := c.Runs.Get(ctx, runID); err == nil {
		c.ingestRun(ctx, run)
	}
}

func (c *ControlPlane) ingestRun(ctx context.Context, run *runs.Run) {
	if run.ConversationID == "" || run.SessionID == "" {
		return
	}
	tool, ok := agentcli.ByName(run.Harness)
	if !ok {
		return
	}
	workingDir := agentcli.TranscriptWorkingDir(run.Harness, run.WorkingDir, run.StartedAt)
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
	turns, consumed, err := agentcli.ParseTranscriptBytes(tool, data)
	if err != nil {
		return
	}
	// The answer to a memory fix is housekeeping, unless the fix came before
	// any model turn for the last prompt. A vendor entry is never a reply.
	// An empty text keeps its turn index and is not stored.
	var fix, unanswered, fixIsReply bool
	if from > 0 {
		fix, unanswered, fixIsReply = cur.memoryFix, cur.unanswered, cur.fixIsReply
	}
	incoming := make([]conversations.IncomingTurn, len(turns))
	for i, t := range turns {
		text := t.Text
		switch {
		case t.Role == conversations.RoleUser && !t.Synthetic:
			if fix = runs.IsMemoryFix(text); fix {
				fixIsReply = unanswered
			} else {
				unanswered, fixIsReply = true, false
			}
		case t.Role == conversations.RoleAssistant && t.Synthetic:
			text = ""
		case t.Role == conversations.RoleAssistant:
			if fix && !fixIsReply {
				text = ""
			}
			text = runs.StripCompletion(text)
			unanswered = false
		}
		incoming[i] = conversations.IncomingTurn{Role: t.Role, Text: text, At: t.At}
	}
	n, err := c.Conversations.IngestAssistantTurnsAt(ctx, run.ID, run.ConversationID, from, incoming)
	if err != nil {
		c.log.Warn("turn ingest", "run_id", run.ID, "error", err)
		return
	}
	if n > 0 && c.Channels != nil && !c.Opts.Shadow {
		// Each closed block goes to the chat now, not at the end of the turn.
		if err := c.Channels.SendReplies(ctx, run.ConversationID, run.ID); err != nil {
			c.log.Warn("channel reply", "run_id", run.ID, "error", err)
		}
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
		path:       path,
		offset:     base + consumed,
		size:       st.Size(),
		mtime:      st.ModTime().UnixNano(),
		turnCount:  from + len(turns),
		memoryFix:  fix,
		unanswered: unanswered,
		fixIsReply: fixIsReply,
	}
	c.turnMu.Unlock()
}

// emitAssistantSettings publishes assistant.settings.updated for an edited
// trebi.yaml. The API settings write emits its own event; the hash the
// write recorded is skipped, so a manual edit is the only source here.
func (c *ControlPlane) emitAssistantSettings(ctx context.Context, ws *workspaces.Workspace) {
	if c.asstResolve == nil || c.Conversations == nil {
		return
	}
	_, hash, err := c.Workspaces.AssistantOverride(ctx, ws.ID)
	if err != nil {
		return
	}
	if !c.Workspaces.AssistantSettingsChanged(ctx, ws.ID, hash) {
		return
	}
	_, _ = c.Store.Append(ctx, store.BusEvent{
		Topic: "assistant.settings.updated", Entity: "workspace", EntityID: ws.ID, Action: "updated",
		Attrs: map[string]string{"workspace": ws.ID},
	})
}
