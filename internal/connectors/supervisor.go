package connectors

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

// Live process statuses (Part B.3).
const (
	statusStopped   = "stopped"
	statusStarting  = "starting"
	statusListening = "listening"
	statusBackoff   = "backoff"
	statusCrashed   = "crashed"
	statusPolling   = "polling"
)

const (
	// unhealthyWindow is the sliding window that counts non-zero exits.
	unhealthyWindow = 5 * time.Minute
	// unhealthyExits is the count that emits connector.unhealthy once.
	unhealthyExits = 4
	backoffBase    = time.Second
	backoffCap     = 60 * time.Second
	pollTimeoutCap = 10 * time.Minute
)

// catchupResult is the last catch-up outcome, kept for the wire row.
type catchupResult struct {
	At       time.Time
	Replayed int
	Reason   string
}

// proc is the live state of one connector supervisor.
type proc struct {
	cancel context.CancelFunc
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
}

type procSnapshot struct {
	status      string
	restarts    int
	events      int64
	duplicates  int64
	parseErrors int64
	lastErr     string
	pid         int
	startedAt   time.Time
	lastEventAt time.Time
	lastCatchup *catchupResult
}

func (p *proc) getStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
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
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill() // process may have already exited
	}
}

func (p *proc) markStarted(at time.Time) {
	p.mu.Lock()
	p.startedAt = at
	p.mu.Unlock()
}

func (p *proc) addEvent() {
	p.mu.Lock()
	p.events++
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

func (p *proc) markEvent(at time.Time) {
	p.mu.Lock()
	p.lastEventAt = at
	p.mu.Unlock()
}

func (p *proc) setLastCatchup(r catchupResult) {
	p.mu.Lock()
	p.lastCatchup = &r
	p.mu.Unlock()
}

// recordExit counts one non-zero exit and reports whether the connector just
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

// clearUnhealthy reports whether the connector was unhealthy; it clears the
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
		lastErr: p.lastErr, startedAt: p.startedAt, lastEventAt: p.lastEventAt,
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

// Wait joins every supervisor goroutine. It must be called after the context
// handed to StartAll is cancelled.
func (s *Service) Wait() { s.wg.Wait() }

// StartAll starts every definition with an events block.
func (s *Service) StartAll(ctx context.Context) {
	_ = s.Load() // start whatever is already in memory if disk reload fails
	s.mu.Lock()
	s.runCtx = ctx
	names := make([]string, 0, len(s.defs))
	for n, d := range s.defs {
		if d.Events != nil {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		if err := s.Start(ctx, n); err != nil {
			s.log.Warn("connector start", "name", n, "error", err)
		}
	}
}

// Start launches one connector supervisor. It reserves the s.procs slot
// under s.mu so concurrent Starts launch one supervisor.
func (s *Service) Start(parent context.Context, name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	s.mu.Lock()
	d, ok := s.defs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	if d.Events == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNoEvents, name)
	}
	runCtx := s.runCtx
	if runCtx == nil {
		runCtx = context.Background()
	}
	if p, ok := s.procs[name]; ok {
		switch p.getStatus() {
		case statusListening, statusStarting, statusBackoff, statusPolling:
			s.mu.Unlock()
			return nil // already live
		}
	}
	ctx, cancel := context.WithCancel(runCtx)
	p := &proc{cancel: cancel, status: statusStarting}
	s.procs[name] = p
	s.mu.Unlock()

	s.wg.Add(1)
	go s.supervise(ctx, name, d, p)
	return nil
}

// Stop kills a connector supervisor and its process.
func (s *Service) Stop(name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	p.cancel()
	p.kill()
	p.setStatus(statusStopped)
	s.log.Info("connector.stopped", "connector", name)
	return nil
}

// Restart stops then starts.
func (s *Service) Restart(ctx context.Context, name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	_ = s.Stop(name) // not-running is the desired start state
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if ok {
		p.addRestart()
	}
	return s.Start(ctx, name)
}

// supervise owns one connector's process lifetime: catch-up once, then the
// live or poll loop with the restart policy.
func (s *Service) supervise(ctx context.Context, name string, d Definition, p *proc) {
	defer s.wg.Done()
	// The proc stays in s.procs after the supervisor ends so the row keeps
	// its terminal status, counters, and last_err.
	defer p.markStopped()
	s.log.Info("connector.started", "connector", name)
	if d.Events.Catchup != nil {
		s.runCatchup(ctx, name, d, p)
	}
	if ctx.Err() != nil {
		return
	}
	if d.Events.Poll > 0 {
		s.runPoll(ctx, name, d, p)
		return
	}
	attempt := 0
	for {
		err := s.runLive(ctx, name, d, p)
		if ctx.Err() != nil {
			return
		}
		failed := err != nil
		if failed {
			p.markCrashed(err.Error())
			s.log.Info("connector.exit", "connector", name, "error", err.Error())
			if p.recordExit(s.now()) {
				s.emitUnhealthy(name, statusCrashed)
			}
		} else {
			p.markStopped()
			s.log.Info("connector.exit", "connector", name, "status", statusStopped)
		}
		if !restartWanted(d.Events.Restart, failed) {
			return
		}
		attempt++
		delay := backoffDelay(attempt)
		p.setStatus(statusBackoff)
		s.log.Info("connector.exit", "connector", name, "backoff", delay.String(), "attempt", attempt)
		if s.sleep(ctx, delay) {
			return
		}
		p.addRestart()
		s.log.Info("connector.restarted", "connector", name, "attempt", attempt)
	}
}

// restartWanted applies the restart policy to one exit.
func restartWanted(policy string, failed bool) bool {
	switch policy {
	case RestartAlways:
		return true
	case RestartNever:
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

// runLive runs the events command and ingests each stdout line until it
// exits. A non-zero exit is returned as an error.
func (s *Service) runLive(ctx context.Context, name string, d Definition, p *proc) error {
	cur, _ := s.GetCursor(ctx, name)
	cmd, stdout, stderr, err := s.startCommand(ctx, d, d.Events.Command, map[string]string{"TREBI_LAST_EVENT_ID": cur.LastEventID})
	if err != nil {
		return err
	}
	p.setCmd(cmd)
	p.setStatus(statusListening)
	p.markStarted(s.now())
	go s.readStderr(name, stderr)
	go s.recoveryTimer(ctx, name, p)
	return s.scanInto(ctx, name, d, stdout, cmd, p, false)
}

// runPoll runs the events command to completion on each interval.
func (s *Service) runPoll(ctx context.Context, name string, d Definition, p *proc) {
	for {
		if ctx.Err() != nil {
			return
		}
		p.setStatus(statusPolling)
		p.markStarted(s.now())
		timeout := d.Events.Poll.Std()
		if timeout > pollTimeoutCap {
			timeout = pollTimeoutCap
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := s.runOnce(cctx, name, d, p)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.markCrashed(err.Error())
			s.log.Info("connector.exit", "connector", name, "error", err.Error())
			if p.recordExit(s.now()) {
				s.emitUnhealthy(name, statusCrashed)
			}
		} else {
			p.setStatus(statusPolling)
			if p.clearUnhealthy() {
				s.emitRecovered(name)
			}
		}
		if s.sleep(ctx, d.Events.Poll.Std()) {
			return
		}
	}
}

// runOnce runs the poll command to completion and ingests its output.
func (s *Service) runOnce(ctx context.Context, name string, d Definition, p *proc) error {
	cur, _ := s.GetCursor(ctx, name)
	cmd, stdout, stderr, err := s.startCommand(ctx, d, d.Events.Command, map[string]string{"TREBI_LAST_EVENT_ID": cur.LastEventID})
	if err != nil {
		return err
	}
	p.setCmd(cmd)
	go s.readStderr(name, stderr)
	return s.scanInto(ctx, name, d, stdout, cmd, p, false)
}

// recoveryTimer emits connector.recovered after five minutes of healthy run
// time, when the connector was unhealthy.
func (s *Service) recoveryTimer(ctx context.Context, name string, p *proc) {
	t := time.NewTimer(unhealthyWindow)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
		if p.clearUnhealthy() {
			s.emitRecovered(name)
		}
	}
}

// startCommand launches sh -c cmdline with the connector env and extra vars.
func (s *Service) startCommand(ctx context.Context, d Definition, cmdline string, extra map[string]string) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	if d.Cwd != "" {
		cmd.Dir = d.Cwd
	}
	env := os.Environ()
	resolved, missing := secrets.ExpandRefs(d.Env)
	for _, secName := range missing {
		s.log.Info("secret.referenced_missing", "name", secName, "connector", d.Name)
	}
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, stderr, nil
}

// scanInto ingests every stdout line, then waits for the command.
func (s *Service) scanInto(ctx context.Context, name string, d Definition, stdout io.Reader, cmd *exec.Cmd, p *proc, catchUp bool) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		appendConnectorLog(name, "stdout", line)
		p.addEvent()
		s.ingest(ctx, name, d, line, catchUp)
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}

func (s *Service) readStderr(name string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		appendConnectorLog(name, "stderr", sc.Text())
	}
}

// runCatchup replays events the connector emitted while the daemon was down.
// It follows the jobi rules: no replay without a connector cursor, a buffer
// cap, a sort by ts when set, and a skip when the cursor is not in the
// output.
func (s *Service) runCatchup(ctx context.Context, name string, d Definition, p *proc) {
	cur, _ := s.GetCursor(ctx, name)
	if cur.LastEventID == "" {
		s.skipCatchup(name, p, "no_position", "catchup skipped: no connector cursor")
		return
	}
	s.emitCatchup(name, "connector.catchup.started", nil)
	cctx, cancel := context.WithTimeout(ctx, d.Events.Catchup.Timeout.Std())
	defer cancel()
	cmd, stdout, stderr, err := s.startCommand(cctx, d, d.Events.Catchup.Command, map[string]string{"TREBI_LAST_EVENT_ID": cur.LastEventID})
	if err != nil {
		s.log.Warn("connector.catchup.started", "connector", name, "error", err)
		s.skipCatchup(name, p, "error", "catchup failed to start: "+err.Error())
		return
	}
	go s.readStderr(name, stderr)
	max := d.Events.Catchup.MaxEvents
	if max <= 0 {
		max = defaultMaxEvents
	}
	var lines []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(lines) >= max {
			if cmd.Process != nil {
				_ = cmd.Process.Kill() // buffer cap reached
			}
			break
		}
		lines = append(lines, sc.Text())
	}
	_ = cmd.Wait() // exit code is irrelevant; the buffer is the result

	type item struct {
		id   string
		ts   time.Time
		line string
	}
	items := make([]item, 0, len(lines))
	for _, line := range lines {
		payload := d.Events.decode(line)
		if payload == nil {
			p.addParseError()
			s.log.Warn("connector.parse_error", "connector", name, "line", truncate(line, 200))
			continue
		}
		it := item{line: line}
		if d.Events.ID != "" {
			it.id, _ = lookupPath(payload, d.Events.ID)
		}
		if d.Events.TS != "" {
			if raw, ok := lookupPath(payload, d.Events.TS); ok {
				it.ts = parseEventTime(raw)
			}
		}
		items = append(items, it)
	}
	if d.Events.TS != "" {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ts.Equal(items[j].ts) {
				return items[i].id < items[j].id
			}
			return items[i].ts.Before(items[j].ts)
		})
	}
	cut := -1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].id == cur.LastEventID && items[i].id != "" {
			cut = i
			break
		}
	}
	if cut < 0 {
		s.skipCatchup(name, p, "position_not_found", "catchup skipped: connector cursor is not in the output")
		return
	}
	replayed := 0
	for _, it := range items[cut+1:] {
		s.ingest(ctx, name, d, it.line, true)
		replayed++
	}
	p.setLastCatchup(catchupResult{At: s.now(), Replayed: replayed})
	s.emitCatchup(name, "connector.catchup.completed", map[string]string{"replayed": strconv.Itoa(replayed)})
}

func (s *Service) skipCatchup(name string, p *proc, reason, logLine string) {
	appendConnectorLog(name, "system", logLine)
	p.setLastCatchup(catchupResult{At: s.now(), Reason: reason})
	s.emitCatchup(name, "connector.catchup.skipped", map[string]string{"reason": reason})
}

// parseEventTime reads RFC3339, epoch seconds, or epoch milliseconds.
func parseEventTime(raw string) time.Time {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1e12 { // epoch milliseconds
			return time.UnixMilli(n).UTC()
		}
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

func truncate(line string, n int) string {
	if len(line) <= n {
		return line
	}
	return line[:n]
}

// emitCatchup appends one catch-up bus event.
func (s *Service) emitCatchup(name, topic string, attrs map[string]string) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	attrs["connector"] = name
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: name, Action: "updated", Attrs: attrs,
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

// emitUnhealthy appends connector.unhealthy once per outage window.
func (s *Service) emitUnhealthy(name, status string) {
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: name, Action: "updated",
		Attrs: map[string]string{"connector": name, "status": status},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.unhealthy", "error", err)
	}
}

// emitRecovered appends connector.recovered.
func (s *Service) emitRecovered(name string) {
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: "connector.recovered", Entity: "connector", EntityID: name, Action: "updated",
		Attrs: map[string]string{"connector": name, "status": statusListening},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.recovered", "error", err)
	}
}

// procByName returns the live proc, or nil.
func (s *Service) procByName(name string) *proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[name]
}
