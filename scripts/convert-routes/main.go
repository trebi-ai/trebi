// Command convert-routes moves the event triggers of JOB.md, AGENT.md, and
// the assistant block of trebi.yaml into routes files. It is a one-time
// program. It runs with the daemon stopped.
//
//	go run ./scripts/convert-routes                    # dry run: report only
//	go run ./scripts/convert-routes --apply            # stop the daemon, back up, convert, check
//	go run ./scripts/convert-routes --home <dir>       # default: $TREBI_HOME or ~/.trebi
//	go run ./scripts/convert-routes --workspace <dir>  # repeatable; default: each workspace in trebi.db
//	go run ./scripts/convert-routes --drop-unroutable  # remove triggers that no route can hold
//
// Exit codes: 0 = done, or nothing to do. 1 = a check failed and nothing
// changed. 2 = the program cannot start (no database, the daemon lock is
// held, a file error).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/routing"
	_ "modernc.org/sqlite" // database/sql driver
)

type options struct {
	home           string
	workspaces     []string
	apply          bool
	dropUnroutable bool
	lockWait       time.Duration
	serviceCmd     []string
}

type app struct {
	o      options
	out    io.Writer
	db     *sql.DB // nil when trebi.db is absent and --workspace is set
	now    func() time.Time
	closer []func()
}

// pathList is a repeatable flag.
type pathList []string

const (
	exitDone    = 0
	exitCheck   = 1
	exitStartup = 2
	defaultWait = 30 * time.Second
)

func (l *pathList) String() string     { return strings.Join(*l, ",") }
func (l *pathList) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("convert-routes", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var o options
	var ws pathList
	fl.BoolVar(&o.apply, "apply", false, "stop the daemon, back up, and write the changes")
	fl.BoolVar(&o.dropUnroutable, "drop-unroutable", false, "remove the event triggers that no route can hold")
	fl.StringVar(&o.home, "home", "", "Trebi home (default $TREBI_HOME or ~/.trebi)")
	fl.Var(&ws, "workspace", "workspace folder; repeatable (default: each workspace in trebi.db)")
	fl.DurationVar(&o.lockWait, "lock-wait", defaultWait, "time to wait for the daemon to stop")
	if err := fl.Parse(args); err != nil {
		return exitStartup
	}
	o.workspaces = ws
	o.serviceCmd = strings.Fields(os.Getenv("TREBI_SERVICE_CMD"))
	if len(o.serviceCmd) == 0 {
		o.serviceCmd = []string{"trebi"}
	}
	a := &app{o: o, out: stdout, now: time.Now}
	code, err := a.run(context.Background())
	for _, c := range a.closer {
		c()
	}
	if err != nil {
		fmt.Fprintln(stderr, "convert-routes:", err)
	}
	return code
}

func (a *app) run(ctx context.Context) (int, error) {
	if a.o.home != "" {
		home.SetBase(a.o.home)
	}
	rows, err := a.loadWorkspaces(ctx)
	if err != nil {
		return exitStartup, err
	}
	if a.o.apply {
		lock, err := a.holdLock(ctx)
		if err != nil {
			return exitStartup, err
		}
		a.closer = append(a.closer, func() { _ = lock.Release() })
	}
	skip := map[string]bool{}
	for _, r := range rows {
		skip[filepath.Clean(r.Path)] = true
	}
	var plans []plan
	for _, r := range rows {
		p, err := convertWorkspace(r, skip)
		if err != nil {
			return exitStartup, err
		}
		plans = append(plans, p)
	}
	blocked := false
	for _, p := range plans {
		a.report(ctx, p)
		blocked = blocked || p.blocked(a.o.dropUnroutable)
	}
	switch {
	case blocked:
		return exitCheck, errors.New("nothing changed: fix the faults above, or use --drop-unroutable for the triggers that no route can hold")
	case !a.o.apply:
		fmt.Fprintln(a.out, "dry run: nothing written. Stop the daemon, then run with --apply.")
		return exitDone, nil
	}
	return a.applyPlans(plans)
}

// loadWorkspaces reads the workspaces to convert.
func (a *app) loadWorkspaces(ctx context.Context) (rows []workspaceRow, err error) {
	dbPath := home.DBPath()
	if _, serr := os.Stat(dbPath); serr == nil {
		db, oerr := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
		if oerr != nil {
			return nil, fmt.Errorf("open %s: %w", dbPath, oerr)
		}
		a.db = db
		a.closer = append(a.closer, func() { _ = db.Close() })
	} else if len(a.o.workspaces) == 0 {
		return nil, fmt.Errorf("no database at %s", dbPath)
	}
	if len(a.o.workspaces) > 0 {
		for _, w := range a.o.workspaces {
			abs, aerr := filepath.Abs(w)
			if aerr != nil {
				return nil, aerr
			}
			rows = append(rows, workspaceRow{Name: filepath.Base(abs), Path: abs})
		}
		return rows, nil
	}
	q, err := a.db.QueryContext(ctx, `SELECT id, name, path FROM workspaces WHERE orphaned = 0 AND archived_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read workspaces: %w", err)
	}
	defer q.Close()
	for q.Next() {
		var r workspaceRow
		if err := q.Scan(&r.ID, &r.Name, &r.Path); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, q.Err()
}

// holdLock stops the daemon when it runs and holds daemon.lock to the end.
func (a *app) holdLock(ctx context.Context) (*home.DaemonLock, error) {
	lock, err := home.AcquireDaemonLock()
	if err == nil {
		return lock, nil
	}
	var running home.AlreadyRunningError
	if !errors.As(err, &running) {
		return nil, err
	}
	fmt.Fprintf(a.out, "daemon: stop with `%s service stop`\n", strings.Join(a.o.serviceCmd, " "))
	cmd := exec.CommandContext(ctx, a.o.serviceCmd[0], append(a.o.serviceCmd[1:], "service", "stop")...) //nolint:gosec // the command comes from the operator
	cmd.Env = append(os.Environ(), "TREBI_HOME="+home.Base())
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		fmt.Fprintf(a.out, "daemon: service stop failed: %v %s\n", cerr, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(a.o.lockWait)
	for {
		if lock, err = home.AcquireDaemonLock(); err == nil {
			return lock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held after %s: %w", home.DaemonLockPath(), a.o.lockWait, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// report prints the plan of one workspace.
func (a *app) report(ctx context.Context, p plan) {
	w := a.out
	fmt.Fprintf(w, "workspace %s (%s)\n", p.ws.Name, p.ws.Path)
	if len(p.routes) == 0 && len(p.unroutable) == 0 && len(p.problems) == 0 {
		fmt.Fprintln(w, "  no event triggers")
		return
	}
	var conns []string
	byConn := map[string][]plannedRoute{}
	for _, r := range p.routes {
		if byConn[r.connection] == nil {
			conns = append(conns, r.connection)
		}
		byConn[r.connection] = append(byConn[r.connection], r)
	}
	sort.Strings(conns)
	for _, c := range conns {
		fmt.Fprintf(w, "  routes/%s.yaml: %d routes, mode off\n", c, len(byConn[c]))
		list := byConn[c]
		sort.SliceStable(list, func(i, j int) bool { return list[i].index < list[j].index })
		for _, r := range list {
			fmt.Fprintf(w, "    %-8s %-40s %-13s %s:%d\n", kindOf(r.route), r.route.TargetName(), r.route.Gets, a.rel(p, r.file), r.line)
		}
		a.queueNote(ctx, p, c)
	}
	for _, u := range p.unroutable {
		fmt.Fprintf(w, "  unroutable: %s:%d event %s: %s\n", a.rel(p, u.file), u.line, u.connection, u.reason)
	}
	for _, s := range p.problems {
		fmt.Fprintf(w, "  fault: %s\n", s)
	}
	for _, f := range p.files {
		if f.source {
			fmt.Fprintf(w, "  edit %s: remove %d event entries\n", f.rel, p.removed[f.path])
		}
	}
}

func (a *app) rel(p plan, path string) string {
	if r, err := filepath.Rel(p.ws.Path, path); err == nil {
		return r
	}
	return path
}

func kindOf(r routing.Route) string {
	switch {
	case r.Assistant:
		return "assistant"
	case r.Job != "":
		return "job"
	}
	return "agent"
}

// queueNote reports the database rows that lose their trigger link. The
// program reads them and changes nothing.
func (a *app) queueNote(ctx context.Context, p plan, connection string) {
	if a.db == nil || p.ws.ID == "" {
		return
	}
	var items, lanes int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_queue WHERE workspace_id = ? AND connector = ? AND status IN ('pending','blocked')`, p.ws.ID, connection).Scan(&items)
	if err == nil {
		err = a.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT q.id) FROM event_queue q JOIN event_queue_batch b ON b.queue_id = q.id WHERE q.workspace_id = ? AND q.connector = ? AND q.status = 'pending'`, p.ws.ID, connection).Scan(&lanes)
	}
	if err != nil {
		fmt.Fprintf(a.out, "    queue: not checked (%v)\n", err)
		return
	}
	if items > 0 || lanes > 0 {
		fmt.Fprintf(a.out, "    queue: %d pending or blocked items and %d open batch lanes lose their trigger link\n", items, lanes)
	}
}

// applyPlans writes the plans. It backs up each changed file first, checks
// the result, and restores every file on a fault.
func (a *app) applyPlans(plans []plan) (int, error) {
	backup := filepath.Join(home.Base(), "backups", "pre-routes-"+a.now().UTC().Format("20060102T150405Z"))
	var restores []func() error
	undo := func() {
		for i := len(restores) - 1; i >= 0; i-- {
			if err := restores[i](); err != nil {
				fmt.Fprintln(a.out, "restore failed:", err)
			}
		}
	}
	changed := 0
	for _, p := range plans {
		if p.empty() {
			continue
		}
		changed++
		restore, err := p.apply(backup)
		if restore != nil {
			restores = append(restores, restore)
		}
		if err != nil {
			undo()
			return exitStartup, err
		}
	}
	if changed == 0 {
		fmt.Fprintln(a.out, "nothing to do")
		return exitDone, nil
	}
	for _, p := range plans {
		if err := p.check(); err != nil {
			undo()
			return exitCheck, fmt.Errorf("the check failed, the files are restored: %w", err)
		}
	}
	fmt.Fprintf(a.out, "done. backup: %s\n", backup)
	return exitDone, nil
}

// apply writes the backup, the routes files, and the source files. On an
// error it restores the files that it wrote. The restore function undoes the
// whole plan.
func (p plan) apply(backupDir string) (restore func() error, err error) {
	var written []fileOp
	restore = func() error {
		var first error
		for i := len(written) - 1; i >= 0; i-- {
			f := written[i]
			var rerr error
			if f.existed {
				rerr = writeAtomic(f.path, f.before, f.mode)
			} else {
				rerr = os.Remove(f.path)
			}
			if rerr != nil && first == nil {
				first = rerr
			}
		}
		return first
	}
	for _, f := range p.files {
		if f.existed {
			dst := filepath.Join(backupDir, safeName(p.ws.Name), f.rel)
			if err := writeNew(dst, f.before, f.mode); err != nil {
				return restore, fmt.Errorf("back up %s: %w", f.path, err)
			}
		}
	}
	for _, f := range p.files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return restore, err
		}
		if err := writeAtomic(f.path, f.after, f.mode); err != nil {
			return restore, fmt.Errorf("write %s: %w", f.path, err)
		}
		written = append(written, f)
	}
	return restore, nil
}

func safeName(s string) string {
	return strings.NewReplacer("/", "_", `\`, "_").Replace(s)
}

// writeNew writes a backup file. It fails when the file exists.
func writeNew(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeAtomic replaces a file with a temporary file and a rename.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), mode)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// check reads the written files back: each routes file must parse with no
// problem, and no changed source may keep an event entry.
func (p plan) check() error {
	for _, f := range p.files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		if f.source {
			if entries, err := eventEntries(data, filepath.Base(f.path) != "trebi.yaml"); err != nil || len(entries) > 0 {
				return fmt.Errorf("%s still has event entries: %v", f.path, err)
			}
			continue
		}
		conn := strings.TrimSuffix(filepath.Base(f.path), ".yaml")
		if probs := routing.Parse(conn, f.path, data).Problems; len(probs) > 0 {
			return fmt.Errorf("%s: %s", f.path, probs[0].Message)
		}
	}
	return nil
}
