package connectors

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/procgroup"
	"github.com/trebi-ai/trebi/internal/store"
)

// Live process statuses (Part B.3). unresponsive is a protocol instance that
// missed three pings.
const (
	statusStopped      = "stopped"
	statusStarting     = "starting"
	statusListening    = "listening"
	statusBackoff      = "backoff"
	statusCrashed      = "crashed"
	statusPolling      = "polling"
	statusUnresponsive = "unresponsive"
)

const (
	// unhealthyWindow is the sliding window that counts non-zero exits.
	unhealthyWindow = 5 * time.Minute
	// unhealthyExits is the count that emits connector.unhealthy once.
	unhealthyExits = 4
	backoffBase    = time.Second
	backoffCap     = 60 * time.Second
	pollTimeoutCap = 10 * time.Minute
	// stopWait bounds the wait of Stop for the supervisor goroutine.
	stopWait = 10 * time.Second
	// shutdownWait bounds the shutdown request; exitWait bounds the wait for
	// the exit after stdin closes (Part D.2).
	shutdownWait = 2 * time.Second
	exitWait     = 5 * time.Second
	// pingMisses is the count of missed pings that makes an instance
	// unresponsive.
	pingMisses  = 3
	replayLimit = 500
)

var errUnresponsive = errors.New("connector missed three pings")

// catchupResult is the last catch-up outcome, kept for the wire row.
type catchupResult struct {
	At       time.Time
	Replayed int
	Reason   string
}

// proc is the live state of one instance supervisor.
type proc struct {
	id     string // connection id
	res    Resolved
	fp     string
	cancel context.CancelFunc
	done   chan struct{} // closed when supervise returns
	mu     sync.Mutex
	cmd    *exec.Cmd

	status      string
	restarts    int
	events      int64
	duplicates  int64
	parseErrors int64
	lastErr     string
	startedAt   time.Time
	lastEventAt time.Time
	lastCatchup *catchupResult

	exits     []time.Time // non-zero exits inside unhealthyWindow
	unhealthy bool
	gated     bool // a required input is missing; no process runs (D13)

	conn *protocol.Conn             // nil until initialize answers
	init *protocol.InitializeResult // the initialize result of the live session
	auth protocol.Status            // the last status notification

	rejected    int64                                 // webhook deliveries the adapter refused
	dropped     int64                                 // webhook deliveries past webhookMaxAge
	subs        map[string]protocol.SubscriptionState // the last sync, by subscription id
	syncedAt    time.Time
	syncMu      sync.Mutex    // one sync at a time
	syncNow     chan struct{} // asks the sync loop for a sync
	syncChanged chan struct{} // subscriptions/changed; throttled
}

// rssTTL caps how often the process table is read. List fills each row.
const rssTTL = 5 * time.Second

// rssTree gives the memory of one process tree in a process table snapshot.
type rssTree interface {
	RSS(ctx context.Context, pid int) (uint64, error)
}

// rssCache keeps one process table snapshot for rssTTL, so the rows of one
// list share one scan, and the memory reading of each pid in it.
type rssCache struct {
	mu    sync.Mutex
	snap  func(context.Context) (rssTree, error) // nil is procgroup.Snapshot
	tree  rssTree
	at    time.Time
	bytes map[int]uint64
}

// read returns the resident memory of pid and its children in bytes. It is
// 0 when pid is 0 or the read fails.
func (c *rssCache) read(pid int) uint64 {
	if pid == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c.bytes == nil || time.Since(c.at) >= rssTTL {
		snap := c.snap
		if snap == nil {
			snap = func(ctx context.Context) (rssTree, error) { return procgroup.Snapshot(ctx) }
		}
		tree, err := snap(ctx)
		if err != nil {
			tree = nil
		}
		c.tree, c.at, c.bytes = tree, time.Now(), map[int]uint64{}
	}
	if n, ok := c.bytes[pid]; ok {
		return n
	}
	var n uint64
	if c.tree != nil {
		n, _ = c.tree.RSS(ctx, pid) // a pid that is gone reads 0
	}
	c.bytes[pid] = n
	return n
}

type procSnapshot struct {
	status      string
	restarts    int
	events      int64
	duplicates  int64
	parseErrors int64
	rejected    int64
	dropped     int64
	lastErr     string
	pid         int
	rssBytes    uint64 // set by the row fill only; 0 is unknown
	startedAt   time.Time
	lastEventAt time.Time
	lastCatchup *catchupResult
	protocol    bool
	init        *protocol.InitializeResult
	auth        protocol.Status
}

// alive reports whether the supervisor goroutine still runs.
func (p *proc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *proc) setStatus(st string) {
	p.mu.Lock()
	p.status = st
	p.mu.Unlock()
}

// markStopped keeps a crashed status; every other status becomes stopped.
func (p *proc) markStopped() {
	p.mu.Lock()
	if p.status != statusCrashed {
		p.status = statusStopped
	}
	p.mu.Unlock()
}

func (p *proc) markCrashed(msg string) {
	p.mu.Lock()
	p.lastErr = msg
	p.status = statusCrashed
	p.mu.Unlock()
}

func (p *proc) setCmd(cmd *exec.Cmd) {
	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()
}

func (p *proc) kill() {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	_ = procgroup.Kill(cmd, syscall.SIGKILL) // the group may have exited
}

func (p *proc) markStarted(at time.Time) {
	p.mu.Lock()
	p.startedAt = at
	p.mu.Unlock()
}

func (p *proc) addDuplicate() {
	p.mu.Lock()
	p.duplicates++
	p.mu.Unlock()
}

func (p *proc) addParseError() {
	p.mu.Lock()
	p.parseErrors++
	p.mu.Unlock()
}

func (p *proc) addRestart() {
	p.mu.Lock()
	p.restarts++
	p.mu.Unlock()
}

// markEvent counts one inserted event and stamps its time.
func (p *proc) markEvent(at time.Time) {
	p.mu.Lock()
	p.events++
	p.lastEventAt = at
	p.mu.Unlock()
}

func (p *proc) setLastCatchup(r catchupResult) {
	p.mu.Lock()
	p.lastCatchup = &r
	p.mu.Unlock()
}

// setSession stores the live session. nil clears it and the last status,
// which a process that does not run cannot vouch for.
func (p *proc) setSession(conn *protocol.Conn, init *protocol.InitializeResult) {
	p.mu.Lock()
	p.conn, p.init = conn, init
	if conn == nil {
		p.auth = protocol.Status{}
	}
	p.mu.Unlock()
}

// session returns the live session, or nil.
func (p *proc) session() (*protocol.Conn, *protocol.InitializeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn, p.init
}

// setAuth stores a status and reports whether the state changed. A status
// with no account keeps the known account.
// setAuth stores a status. It reports a change of the state and a change of
// the links. A status with no links keeps the last links.
func (p *proc) setAuth(st protocol.Status) (changed, linksChanged bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st.Account == nil && p.auth.Account != nil && st.State == protocol.StateConnected {
		st.Account = p.auth.Account
	}
	if st.Links == nil {
		st.Links = p.auth.Links
	}
	changed = p.auth.State != st.State || p.auth.Reason != st.Reason || !slices.Equal(p.auth.LoggedOut(), st.LoggedOut())
	linksChanged = !slices.Equal(p.auth.Links, st.Links)
	p.auth = st
	return changed, linksChanged
}

func (p *proc) authStatus() protocol.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auth
}

// recordExit counts one non-zero exit and reports whether the instance just
// crossed into unhealthy (four exits in five minutes, emitted once).
func (p *proc) recordExit(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exits = append(p.exits, now)
	cut := now.Add(-unhealthyWindow)
	kept := p.exits[:0]
	for _, t := range p.exits {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	p.exits = kept
	if len(p.exits) >= unhealthyExits && !p.unhealthy {
		p.unhealthy = true
		return true
	}
	return false
}

// clearUnhealthy reports whether the instance was unhealthy; it clears the
// flag so the next outage emits connector.unhealthy again.
func (p *proc) clearUnhealthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := p.unhealthy
	p.unhealthy = false
	return was
}

func (p *proc) snapshot() procSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := procSnapshot{
		status: p.status, restarts: p.restarts, events: p.events,
		duplicates: p.duplicates, parseErrors: p.parseErrors,
		rejected: p.rejected, dropped: p.dropped,
		lastErr: p.lastErr, startedAt: p.startedAt, lastEventAt: p.lastEventAt,
		protocol: p.res.Manifest.Protocol() == manifest.ProtocolConnector,
		init:     p.init, auth: p.auth,
	}
	if p.cmd != nil && p.cmd.Process != nil {
		snap.pid = p.cmd.Process.Pid
	}
	if p.lastCatchup != nil {
		cp := *p.lastCatchup
		snap.lastCatchup = &cp
	}
	return snap
}

// running reports whether the status has a live process. The pid stays
// after the process ends.
func (snap procSnapshot) running() bool {
	switch snap.status {
	case statusStopped, statusBackoff, statusCrashed:
		return false
	}
	return snap.pid != 0
}

// fill writes the live fields of one instance row.
func (snap procSnapshot) fill(row map[string]any) {
	row["status"] = snap.status
	row["restarts"] = snap.restarts
	row["last_err"] = snap.lastErr
	row["counters"] = map[string]any{
		"events": snap.events, "duplicates": snap.duplicates, "parse_errors": snap.parseErrors,
		"rejected": snap.rejected, "dropped": snap.dropped,
	}
	if snap.pid != 0 {
		row["pid"] = snap.pid
	}
	if snap.rssBytes > 0 {
		row["rss_bytes"] = snap.rssBytes
	}
	if !snap.startedAt.IsZero() {
		row["started_at"] = snap.startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !snap.lastEventAt.IsZero() {
		row["last_event_at"] = snap.lastEventAt.UTC().Format(time.RFC3339Nano)
	}
	if snap.lastCatchup != nil {
		row["last_catchup"] = map[string]any{
			"at":             snap.lastCatchup.At.UTC().Format(time.RFC3339Nano),
			"replayed":       snap.lastCatchup.Replayed,
			"skipped_reason": snap.lastCatchup.Reason,
		}
	}
	if snap.protocol {
		row["auth"] = snap.authRow()
		if snap.init != nil && snap.init.Account != nil {
			row["account"] = snap.init.Account
		}
	}
}

// authRow is the wire form of the adapter state. It is nil for a lines
// instance.
func (snap procSnapshot) authRow() map[string]any {
	if !snap.protocol {
		return nil
	}
	row := map[string]any{
		"state": snap.auth.State, "reason": snap.auth.Reason, "message": snap.auth.Message,
		"account": snap.auth.Account, "accounts": orEmptySlice(snap.auth.Accounts),
	}
	if snap.auth.State == "" {
		row["state"] = protocol.StateConnecting
	}
	return row
}

// features returns what the process delivers, or nil before initialize.
func (snap procSnapshot) features() []string {
	if snap.init == nil {
		return nil
	}
	return snap.init.Features
}

// limits returns the delivered limits, else the promised ones.
func (snap procSnapshot) limits(m manifest.Manifest) manifest.Limits {
	if snap.init != nil && (snap.init.Limits.MaxText > 0 || len(snap.init.Limits.Formats) > 0) {
		return manifest.Limits{MaxText: snap.init.Limits.MaxText, Formats: snap.init.Limits.Formats}
	}
	if m.Channel != nil {
		return m.Channel.Limits
	}
	return manifest.Limits{}
}

// Wait joins every supervisor goroutine, or returns ctx.Err() when ctx ends
// first. Call it after StopAll or after the StartAll context is cancelled.
func (s *Service) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { // ends when the supervisors end
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StopAll cancels every supervisor and kills its process group at once.
func (s *Service) StopAll() {
	s.mu.Lock()
	procs := make([]*proc, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.started = false
	s.mu.Unlock()
	for _, p := range procs {
		if p.cancel != nil {
			p.cancel()
		}
		p.kill()
	}
}

// StartAll installs missing packages and starts every connection with an
// events block. Later writes start and stop connections through reconcile.
func (s *Service) StartAll(ctx context.Context) {
	if err := s.Load(); err != nil {
		s.log.Warn("connections", "error", err) // start what loaded
	}
	s.SweepStaging()
	s.sweepGroups(ctx)
	s.EnsurePackages(ctx)
	s.mu.Lock()
	s.runCtx = ctx
	s.started = true
	s.mu.Unlock()
	s.wg.Add(1)
	go s.runWebhookQueue(ctx)
	s.reconcile(ctx)
}

// Start launches the supervisor of one connection. Backup and restore call
// it after Stop.
func (s *Service) Start(_ context.Context, ref string) error {
	if err := s.GuardReserved(ref); err != nil {
		return err
	}
	if err := s.GuardMutate(ref); err != nil {
		return err
	}
	r, err := s.Resolve(ref, "")
	if err != nil {
		return err
	}
	if r.Manifest.Events == nil {
		return fmt.Errorf("%w: %q", ErrNoEvents, ref)
	}
	s.startInstance(r)
	return nil
}

// startInstance reserves the s.procs slot under s.mu, so concurrent starts
// launch one supervisor. A connection with a missing required input does not
// start (D13): its proc records auth_required and the process stays down.
func (s *Service) startInstance(r Resolved) {
	s.mu.Lock()
	runCtx := s.runCtx
	p := &proc{
		id: r.ID, res: r, fp: r.fingerprint(), status: statusStarting, done: make(chan struct{}),
		syncNow: make(chan struct{}, 1), syncChanged: make(chan struct{}, 1),
	}
	if old, ok := s.procs[r.ID]; ok {
		if old.alive() {
			s.mu.Unlock()
			return
		}
		p.restarts = old.snapshot().restarts
	}
	if missing := r.MissingInputs(); len(missing) > 0 {
		label := cmp.Or(missing[0].Label, missing[0].Name)
		p.gated = true
		p.status = statusStopped
		p.lastErr = label + " is missing"
		if slices.Contains(r.Manifest.Setup.KeyInputs(), missing[0].Name) {
			p.lastErr = "Log in first."
		}
		p.auth = protocol.Status{State: protocol.StateAuthRequired, Reason: protocol.ReasonMissingInput, Message: p.lastErr}
		close(p.done)
		s.procs[r.ID] = p
		s.mu.Unlock()
		s.emitConnector("connector.status", r.ID, map[string]string{"state": protocol.StateAuthRequired, "reason": protocol.ReasonMissingInput})
		return
	}
	ctx, cancel := context.WithCancel(runCtx)
	p.cancel = cancel
	s.procs[r.ID] = p
	s.mu.Unlock()

	s.wg.Add(1)
	go s.supervise(ctx, p)
}

// Stop ends the supervisor of one connection. A protocol adapter gets
// shutdown first. It waits for the supervisor to end.
func (s *Service) Stop(ref string) error {
	if err := s.GuardReserved(ref); err != nil {
		return err
	}
	if err := s.GuardMutate(ref); err != nil {
		return err
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return err
	}
	s.stopInstance(c.ID)
	return nil
}

func (s *Service) stopInstance(id string) {
	p := s.procByName(id)
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
	case <-time.After(stopWait):
		p.kill()
		s.log.Warn("connector.stopped", "connector", id, "error", "supervisor did not end")
	}
	p.setStatus(statusStopped)
	s.log.Info("connector.stopped", "connector", id)
}

// Restart stops then starts one connection.
func (s *Service) Restart(ctx context.Context, ref string) error {
	if err := s.Stop(ref); err != nil {
		return err
	}
	if err := s.Start(ctx, ref); err != nil {
		return err
	}
	if p := s.procByName(s.IDOf(ref)); p != nil {
		p.addRestart()
	}
	return nil
}

// reconcile moves the live processes to the resolved set after a write: it
// stops a removed connection, restarts a changed one, and starts a new one
// once StartAll ran. A connection that a user stopped stays stopped. A
// connection that waits for an input starts when its inputs change.
func (s *Service) reconcile(ctx context.Context) {
	want := map[string]Resolved{}
	for _, r := range s.Instances() {
		want[r.ID] = r
	}
	s.mu.Lock()
	started := s.started
	procs := maps.Clone(s.procs)
	s.mu.Unlock()
	for id, p := range procs {
		r, ok := want[id]
		switch {
		case !ok:
			s.stopInstance(id)
			s.mu.Lock()
			if s.procs[id] == p {
				delete(s.procs, id)
			}
			s.mu.Unlock()
		case p.fp == r.fingerprint():
		case p.alive():
			s.log.InfoContext(ctx, "connector.restarted", "connector", id, "reason", "changed")
			s.stopInstance(id)
			s.startInstance(r)
			if np := s.procByName(id); np != nil {
				np.addRestart()
			}
		case p.gated && started:
			s.startInstance(r)
		}
	}
	if started {
		for id, r := range want {
			if _, ok := procs[id]; !ok {
				s.startInstance(r)
			}
		}
	}
	if s.hooks != nil {
		s.mu.Lock()
		conns := maps.Clone(s.conns)
		s.mu.Unlock()
		s.hooks.EnsureHooks(context.WithoutCancel(ctx), hookWants(slices.Collect(maps.Values(want)), conns))
	}
	s.syncCommandLinks(ctx)
}

// supervise owns one instance's process lifetime: the run loop with the
// restart policy. A lines instance runs catch-up before each start.
func (s *Service) supervise(ctx context.Context, p *proc) {
	defer s.wg.Done()
	defer close(p.done)
	// The proc stays in s.procs after the supervisor ends so the row keeps
	// its terminal status, counters, and last_err.
	defer p.markStopped()
	ev := p.res.Manifest.Events
	s.log.Info("connector.started", "connector", p.id)
	lines := ev.Protocol != manifest.ProtocolConnector
	if lines && ev.Poll > 0 {
		if ev.Catchup != nil {
			s.runCatchup(ctx, p)
		}
		s.runPoll(ctx, p)
		return
	}
	attempt := 0
	for {
		if lines && ev.Catchup != nil {
			s.runCatchup(ctx, p)
		}
		if ctx.Err() != nil {
			return
		}
		var healthy bool
		var err error
		if lines {
			healthy, err = s.runLines(ctx, p)
		} else {
			healthy, err = s.runProtocol(ctx, p)
		}
		if ctx.Err() != nil {
			return
		}
		if healthy {
			attempt = 0
		}
		failed := err != nil
		if failed {
			p.markCrashed(err.Error())
			s.log.Info("connector.exit", "connector", p.id, "error", err.Error())
			if p.recordExit(s.now()) {
				s.emitUnhealthy(p.id, statusCrashed)
			}
		} else {
			p.markStopped()
			s.log.Info("connector.exit", "connector", p.id, "status", statusStopped)
		}
		if !restartWanted(ev.Restart, failed) {
			return
		}
		attempt++
		delay := backoffDelay(attempt)
		p.setStatus(statusBackoff)
		s.log.Info("connector.exit", "connector", p.id, "backoff", delay.String(), "attempt", attempt)
		if s.sleep(ctx, delay) {
			return
		}
		p.addRestart()
		s.log.Info("connector.restarted", "connector", p.id, "attempt", attempt)
	}
}

// restartWanted applies the restart policy to one exit.
func restartWanted(policy string, failed bool) bool {
	switch policy {
	case manifest.RestartAlways:
		return true
	case manifest.RestartNever:
		return false
	default: // on-failure
		return failed
	}
}

// backoffDelay is 1s << (attempt-1), capped at 60s.
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	d := backoffBase << (attempt - 1)
	if d > backoffCap {
		d = backoffCap
	}
	return d
}

// goHealthy sets the returned flag when the run lasts the health window, and
// then emits connector.recovered once.
func (s *Service) goHealthy(ctx context.Context, p *proc) *atomic.Bool {
	var ok atomic.Bool
	s.wg.Add(1)
	go func() { // ends with the run
		defer s.wg.Done()
		if s.sleep(ctx, unhealthyWindow) {
			return
		}
		ok.Store(true)
		if p.clearUnhealthy() {
			s.emitRecovered(p.id)
		}
	}()
	return &ok
}

// runLines runs the events command and ingests each stdout line until it
// exits. A non-zero exit is returned as an error.
func (s *Service) runLines(ctx context.Context, p *proc) (healthy bool, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pos, _ := p.res.Folder().Cursor()
	cmd, stdout, stderr, err := s.startCommand(runCtx, p.res, p.res.Manifest.Events.Command, map[string]string{"TREBI_LAST_EVENT_ID": pos.LastEventID}, nil)
	if err != nil {
		return false, err
	}
	p.setCmd(cmd)
	p.setStatus(statusListening)
	p.markStarted(s.now())
	s.goStderr(p.id, stderr)
	ok := s.goHealthy(runCtx, p)
	err = s.scanInto(ctx, p, stdout, cmd, false)
	cancel()
	return ok.Load(), err
}

// runPoll runs the events command to completion on each interval.
func (s *Service) runPoll(ctx context.Context, p *proc) {
	every := p.res.Manifest.Events.Poll.Std()
	for {
		if ctx.Err() != nil {
			return
		}
		p.setStatus(statusPolling)
		p.markStarted(s.now())
		cctx, cancel := context.WithTimeout(ctx, min(every, pollTimeoutCap))
		err := s.runOnce(cctx, p)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.markCrashed(err.Error())
			p.setStatus(statusBackoff) // crashed means the supervisor ended
			s.log.Info("connector.exit", "connector", p.id, "error", err.Error())
			if p.recordExit(s.now()) {
				s.emitUnhealthy(p.id, statusCrashed)
			}
		} else if p.clearUnhealthy() {
			s.emitRecovered(p.id)
		}
		if s.sleep(ctx, every) {
			return
		}
	}
}

// runOnce runs the poll command to completion and ingests its output.
func (s *Service) runOnce(ctx context.Context, p *proc) error {
	pos, _ := p.res.Folder().Cursor()
	cmd, stdout, stderr, err := s.startCommand(ctx, p.res, p.res.Manifest.Events.Command, map[string]string{"TREBI_LAST_EVENT_ID": pos.LastEventID}, nil)
	if err != nil {
		return err
	}
	p.setCmd(cmd)
	s.goStderr(p.id, stderr)
	return s.scanInto(ctx, p, stdout, cmd, false)
}

// runProtocol runs one trebi-connector/1 session: initialize, replay,
// initialized, then pings until the process ends or ctx ends. On ctx end it
// sends shutdown, closes stdin, and kills the group after exitWait.
func (s *Service) runProtocol(ctx context.Context, p *proc) (healthy bool, err error) {
	// The process context is not ctx, so a stop can shut down in order.
	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	pr, pw, err := os.Pipe()
	if err != nil {
		return false, err
	}
	cmd, stdout, stderr, err := s.startCommand(pctx, p.res, p.res.Manifest.Events.Command,
		map[string]string{"TREBI_PROTOCOL": protocol.Version}, pr)
	_ = pr.Close() // the child holds its own copy
	if err != nil {
		_ = pw.Close() // no process reads it
		return false, err
	}
	p.setCmd(cmd)
	p.setStatus(statusStarting)
	p.markStarted(s.now())
	s.goStderr(p.id, stderr)
	conn := protocol.NewConn(stdout, pw, func(method string, params json.RawMessage) { s.onNotify(p, method, params) },
		protocol.WithBadLine(func(err error) {
			p.addParseError()
			s.log.Warn("connector.parse_error", "connector", p.id, "reason", "not_jsonrpc", "error", err)
		}))
	s.wg.Add(1)
	go func() { // ends at stdout EOF, when the process group exits
		defer s.wg.Done()
		if err := conn.Run(context.Background()); err != nil && errors.Is(err, protocol.ErrLineTooLong) {
			p.addParseError()
			s.log.Warn("connector.parse_error", "connector", p.id, "reason", "line_too_long")
			pcancel()
		}
	}()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ok := s.goHealthy(runCtx, p)
	err = s.session(ctx, p, conn)
	p.setSession(nil, nil)
	if ctx.Err() != nil {
		sctx, scancel := context.WithTimeout(context.Background(), shutdownWait)
		_ = conn.Call(sctx, protocol.MethodShutdown, protocol.Empty{}, nil) // the exit below is the result
		scancel()
	}
	_ = pw.Close() // EOF on stdin asks the adapter to exit
	if err != nil && ctx.Err() == nil {
		pcancel()
	}
	select {
	case <-conn.Done():
	case <-time.After(exitWait):
		pcancel()
		<-conn.Done()
	}
	waitErr := cmd.Wait()
	if err == nil {
		err = waitErr
	}
	return ok.Load(), err
}

// session drives one live protocol session until the process ends, ctx
// ends, or the adapter misses three pings.
func (s *Service) session(ctx context.Context, p *proc, conn *protocol.Conn) error {
	r := p.res
	pos, _ := p.res.Folder().Cursor()
	var init protocol.InitializeResult
	ictx, cancel := context.WithTimeout(ctx, protocol.DefaultTimeout)
	err := conn.Call(ictx, protocol.MethodInitialize, protocol.InitializeParams{
		Protocol:    protocol.Version,
		Daemon:      protocol.DaemonInfo{Version: s.version},
		Instance:    protocol.InstanceInfo{Key: r.ID, Name: r.Type},
		LastEventID: pos.LastEventID,
		Webhook:     r.Webhook,
	}, &init)
	cancel()
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if init.Protocol != protocol.Version {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.id, "reason", "protocol_mismatch", "protocol", init.Protocol)
		return fmt.Errorf("adapter speaks %q, not %s", init.Protocol, protocol.Version)
	}
	p.setSession(conn, &init)
	if pos.LastEventID != "" && slices.Contains(init.Features, manifest.FeatureReplay) {
		s.replay(ctx, p, conn, pos.LastEventID)
	}
	if err := conn.Notify(ctx, protocol.MethodInitialized, protocol.Empty{}); err != nil {
		return err
	}
	p.setStatus(statusListening)
	s.wakeWebhooks()
	if slices.Contains(init.Features, manifest.FeatureSubscriptions) {
		p.kick(p.syncNow)
		s.wg.Add(1)
		go func() { // ends with the session
			defer s.wg.Done()
			s.runSyncLoop(ctx, p, conn)
		}()
	}
	ping := time.NewTicker(s.pingEvery)
	defer ping.Stop()
	authTick := time.NewTicker(s.authEvery)
	defer authTick.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-conn.Done():
			return conn.Err()
		case <-ping.C:
			err := conn.Call(ctx, protocol.MethodPing, protocol.Empty{}, nil)
			switch {
			case err == nil:
				misses = 0
			case ctx.Err() != nil || errors.Is(err, protocol.ErrClosed):
			default:
				misses++
				if misses >= pingMisses {
					p.setStatus(statusUnresponsive)
					s.emitStatus(p, statusUnresponsive, "")
					return errUnresponsive
				}
			}
		case <-authTick.C:
			if p.authStatus().State != protocol.StateConnected {
				continue
			}
			var st protocol.AuthStatusResult
			if err := conn.Call(ctx, protocol.MethodAuthStatus, protocol.Empty{}, &st); err == nil && st.State != "" {
				s.applyStatus(p, protocol.Status{State: st.State, Account: st.Account})
			}
		}
	}
}

// replay asks for the events after the connector cursor before initialized
// (Part D.5). An incomplete replay is logged as a skip.
func (s *Service) replay(ctx context.Context, p *proc, conn *protocol.Conn, after string) {
	s.emitCatchup(p.id, "connector.catchup.started", nil)
	var res protocol.ReplayResult
	if err := conn.Call(ctx, protocol.MethodEventsReplay, protocol.ReplayParams{After: after, Limit: replayLimit}, &res); err != nil {
		s.skipCatchup(p, "error", "catchup failed: "+err.Error())
		return
	}
	if !res.Complete {
		s.skipCatchup(p, "cursor_gone", "catchup skipped: the adapter no longer has the connector cursor")
		return
	}
	for _, ev := range res.Events {
		s.ingestProtocol(ctx, p, ev, true)
	}
	p.setLastCatchup(catchupResult{At: s.now(), Replayed: len(res.Events)})
	s.emitCatchup(p.id, "connector.catchup.completed", map[string]string{"replayed": strconv.Itoa(len(res.Events))})
}

// startCommand launches sh -c cmdline with the process env and extra vars.
func (s *Service) startCommand(ctx context.Context, r Resolved, cmdline string, extra map[string]string, stdin io.Reader) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	if err := r.Folder().Create(); err != nil {
		return nil, nil, nil, err
	}
	env := s.processEnv(r)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	cmd, stdout, stderr, err := procgroup.Command{Script: cmdline, Dir: r.WorkDir(), Env: env, Stdin: stdin, WaitDelay: exitWait}.Start(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if m, err := procgroup.MarkOf(ctx, cmd); err == nil {
		_ = r.Folder().SetGroup(m) //nolint:errcheck // without the record, only the boot sweep of this group is lost
	}
	return cmd, stdout, stderr, nil
}

// processEnv is the env of a connection program: the base env (D12), the
// inputs, and the connection folders. PATH starts with the bin folder of
// the version.
func (s *Service) processEnv(r Resolved) []string {
	env := protocol.BaseEnv(os.Environ())
	vals, missing := r.Env()
	for _, secName := range missing {
		s.log.Info("secret.referenced_missing", "name", secName, "connector", r.ID)
	}
	for k, v := range vals {
		env = append(env, k+"="+v)
	}
	return append(env,
		"PATH="+r.PathEnv(os.Getenv("PATH")),
		"TMPDIR="+r.CacheDir(),
		"TREBI_STATE_DIR="+r.StateDir(),
		"TREBI_CACHE_DIR="+r.CacheDir(),
	)
}

// sweepGroups kills each process group that a daemon before this one
// started and left running, for example after a crash. Otherwise a second
// copy of the program starts next to it.
func (s *Service) sweepGroups(ctx context.Context) {
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.conns))
	for id := range s.loadErr {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		if m, ok := NewFolder(id).TakeGroup(); ok && m.KillLeft(ctx) {
			s.log.Warn("connector.exit", "connector", id, "reason", "left_by_old_daemon", "pgid", m.PGID)
		}
	}
}

// scanInto ingests every stdout line, then waits for the command. A scan
// error kills the process group first, so the child cannot block on a full
// pipe.
func (s *Service) scanInto(ctx context.Context, p *proc, stdout io.Reader, cmd *exec.Cmd, catchUp bool) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLine)
	for sc.Scan() {
		line := sc.Text()
		s.appendLog(p.id, "stdout", line)
		s.ingest(ctx, p, line, catchUp)
	}
	scanErr := sc.Err()
	if scanErr != nil {
		if errors.Is(scanErr, bufio.ErrTooLong) {
			p.addParseError()
			s.log.Warn("connector.parse_error", "connector", p.id, "reason", "line_too_long")
		}
		_ = procgroup.Kill(cmd, syscall.SIGKILL) // the group may have exited
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}

// goStderr copies stderr to the stream log in a goroutine that s.wg tracks.
// It ends at EOF, when the process group exits.
func (s *Service) goStderr(id string, r io.Reader) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		br := bufio.NewReaderSize(r, logLineCap)
		var line []byte
		for {
			chunk, err := br.ReadSlice('\n')
			if len(line) <= logLineCap {
				line = append(line, chunk...) // AppendLog cuts a long line; the rest of it is dropped
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if len(line) > 0 {
				s.appendLog(id, "stderr", strings.TrimRight(string(line), "\r\n"))
			}
			line = line[:0]
			if err != nil {
				return // EOF or close
			}
		}
	}()
}

// runCatchup replays the events a lines connector emitted while the daemon
// was down: no replay without a connector cursor, a buffer cap, a sort by ts
// when set, and a skip when the cursor is not in the output.
func (s *Service) runCatchup(ctx context.Context, p *proc) {
	ev := p.res.Manifest.Events
	pos, _ := p.res.Folder().Cursor()
	if pos.LastEventID == "" {
		s.skipCatchup(p, "no_position", "catchup skipped: no connector cursor")
		return
	}
	s.emitCatchup(p.id, "connector.catchup.started", nil)
	cctx, cancel := context.WithTimeout(ctx, ev.Catchup.Timeout.Std())
	defer cancel()
	cmd, stdout, stderr, err := s.startCommand(cctx, p.res, ev.Catchup.Command, map[string]string{"TREBI_LAST_EVENT_ID": pos.LastEventID}, nil)
	if err != nil {
		s.log.Warn("connector.catchup.started", "connector", p.id, "error", err)
		s.skipCatchup(p, "error", "catchup failed to start: "+err.Error())
		return
	}
	s.goStderr(p.id, stderr)
	lines := readCapped(stdout, ev.Catchup.MaxEvents, cmd)
	_ = cmd.Wait() // exit code is irrelevant; the buffer is the result

	type item struct {
		id   string
		ts   time.Time
		line string
	}
	items := make([]item, 0, len(lines))
	for _, line := range lines {
		payload := decodeLine(ev, line)
		if payload == nil {
			p.addParseError()
			s.log.Warn("connector.parse_error", "connector", p.id, "line", truncate(line, 200))
			continue
		}
		it := item{line: line}
		it.id, _ = lookupPath(payload, ev.ID)
		if raw, ok := lookupPath(payload, ev.TS); ok {
			it.ts = parseEventTime(raw)
		}
		items = append(items, it)
	}
	if ev.TS != "" {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ts.Equal(items[j].ts) {
				return items[i].id < items[j].id
			}
			return items[i].ts.Before(items[j].ts)
		})
	}
	cut := -1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].id == pos.LastEventID && items[i].id != "" {
			cut = i
			break
		}
	}
	if cut < 0 {
		s.skipCatchup(p, "position_not_found", "catchup skipped: connector cursor is not in the output")
		return
	}
	replayed := 0
	for _, it := range items[cut+1:] {
		s.ingest(ctx, p, it.line, true)
		replayed++
	}
	p.setLastCatchup(catchupResult{At: s.now(), Replayed: replayed})
	s.emitCatchup(p.id, "connector.catchup.completed", map[string]string{"replayed": strconv.Itoa(replayed)})
}

// readCapped reads at most max lines. Over the cap, or on a scan error, it
// kills the writer before the caller waits.
func readCapped(r io.Reader, max int, cmd *exec.Cmd) []string {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLine)
	for sc.Scan() {
		if len(lines) >= max {
			_ = procgroup.Kill(cmd, syscall.SIGKILL) // stop the writer before Wait
			return lines
		}
		lines = append(lines, sc.Text())
	}
	if sc.Err() != nil {
		_ = procgroup.Kill(cmd, syscall.SIGKILL) // stop the writer before Wait
	}
	return lines
}

func (s *Service) skipCatchup(p *proc, reason, logLine string) {
	s.appendLog(p.id, "system", logLine)
	p.setLastCatchup(catchupResult{At: s.now(), Reason: reason})
	s.emitCatchup(p.id, "connector.catchup.skipped", map[string]string{"reason": reason})
}

// parseEventTime reads RFC3339, epoch seconds, or epoch milliseconds. Epoch
// values can have a fraction, such as the Slack ts "1727380000.123456".
func parseEventTime(raw string) time.Time {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	whole, frac, _ := strings.Cut(raw, ".")
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || n < 0 {
		return time.Time{}
	}
	unit := int64(time.Second)
	if n > 1e12 { // epoch milliseconds
		unit = int64(time.Millisecond)
	}
	var sub int64
	if frac != "" {
		if len(frac) > 9 {
			frac = frac[:9]
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil || f < 0 || strings.ContainsAny(frac, "+-") {
			return time.Time{}
		}
		for i := len(frac); i < 9; i++ {
			f *= 10
		}
		sub = f * unit / int64(time.Second)
	}
	return time.Unix(0, n*unit+sub).UTC()
}

func truncate(line string, n int) string {
	if len(line) <= n {
		return line
	}
	return line[:n]
}

// emitConnector appends one connector bus event keyed by the connection id.
func (s *Service) emitConnector(topic, id string, attrs map[string]string) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	s.mu.Lock()
	c := s.conns[id]
	s.mu.Unlock()
	attrs["connector"] = c.Type
	attrs["instance"] = id
	attrs["slug"] = c.Slug
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: id, Action: "updated", Attrs: attrs,
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

// emitCatchup appends one catch-up bus event.
func (s *Service) emitCatchup(id, topic string, attrs map[string]string) {
	s.emitConnector(topic, id, attrs)
}

// emitUnhealthy appends connector.unhealthy once per outage window.
func (s *Service) emitUnhealthy(id, status string) {
	s.emitConnector("connector.unhealthy", id, map[string]string{"status": status})
}

// emitRecovered appends connector.recovered.
func (s *Service) emitRecovered(id string) {
	s.emitConnector("connector.recovered", id, map[string]string{"status": statusListening})
}

// emitStatus appends connector.status and tells the status hook.
func (s *Service) emitStatus(p *proc, state, reason string) {
	s.emitConnector("connector.status", p.id, map[string]string{"state": state, "reason": reason})
	if s.onStatus != nil {
		s.onStatus(context.Background(), p.id, state)
	}
}

// procByName returns the live proc of a connection id, or nil.
func (s *Service) procByName(id string) *proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[id]
}
