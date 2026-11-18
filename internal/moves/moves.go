// Package moves moves a workspace from this machine to another machine of
// the account: the folder, the store rows, the run files, and the secrets.
package moves

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/machines"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Phases of a move, in order. The bus topic workspace.move carries each.
const (
	PhaseArchive = "archive"
	PhaseSend    = "send"
	PhaseRestore = "restore"
	PhaseCloud   = "cloud"
	PhaseDone    = "done"
	PhaseFailed  = "failed"
)

// Errors of Start.
var (
	ErrSelf     = errors.New("the workspace is on this machine already")
	ErrBusy     = errors.New("a run of this workspace is running or waiting; wait for it to finish")
	ErrInFlight = errors.New("a move of this workspace is in progress")
	ErrArchived = errors.New("this workspace moved to another machine")
	ErrNoDevice = errors.New("the machine is not linked to the cloud")
)

// Dialer opens the HTTP transport to another machine of the account.
type Dialer func(ctx context.Context, machineID string) (http.RoundTripper, error)

// Cloud moves the cloud rows of a workspace to another device.
type Cloud interface {
	MoveWorkspace(ctx context.Context, workspaceID string, req cloudlink.WorkspaceMoveRequest) (*cloudlink.WorkspaceMoveResult, error)
}

// Machines finds the machines of the account.
type Machines interface {
	Get(ctx context.Context, id string) (machines.Machine, error)
	List(ctx context.Context) ([]machines.Machine, error)
	Self(ctx context.Context) (machines.Machine, error)
}

// Options wires a Service. Home and Secrets default to the Trebi home.
type Options struct {
	Machines Machines
	Dial     Dialer
	Cloud    Cloud
	Home     string
	Secrets  secrets.Dir
	// FailRestore makes Import fail after it wrote the files (tests only).
	FailRestore bool
}

// Move is one move of a workspace to a machine.
type Move struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	MachineID   string    `json:"machine_id"`
	Machine     string    `json:"machine"`
	Phase       string    `json:"phase"`
	Error       string    `json:"error,omitempty"`
	Restored    bool      `json:"restored"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Service starts moves on the source machine and restores them on the
// target machine.
type Service struct {
	st   *store.Store
	ws   *workspaces.Service
	opts Options
	log  *slog.Logger

	ctx    context.Context // process lifetime; Close cancels it
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	active map[string]bool // workspace ids with a move in progress
}

// New builds the move service.
func New(st *store.Store, ws *workspaces.Service, opts Options) *Service {
	if opts.Home == "" {
		opts.Home = home.Base()
	}
	if opts.Secrets == "" {
		opts.Secrets = secrets.HomeDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		st: st, ws: ws, opts: opts, log: slog.Default().With("source", "moves"),
		ctx: ctx, cancel: cancel, active: map[string]bool{},
	}
}

// Close stops the moves in progress and waits for them.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

// Start checks a move of workspace wsID to the machine ref (id or name)
// and runs it in the background. It reuses an unfinished move to the same
// machine, so a retry after a failed cloud step skips the restore.
func (s *Service) Start(ctx context.Context, wsID, ref string) (*Move, error) { //nolint:gocyclo // each check is one refusal that the API maps to a code
	ws, err := s.ws.Get(ctx, wsID)
	if err != nil {
		return nil, err
	}
	m, err := s.machine(ctx, ref)
	if err != nil {
		return nil, err
	}
	if m.IsSelf {
		return nil, ErrSelf
	}
	if m.DeviceID == nil || *m.DeviceID == "" {
		return nil, ErrNoDevice
	}
	prev, err := s.latest(ctx, ws.ID, m.ID)
	if err != nil {
		return nil, err
	}
	retry := prev != nil && prev.Phase == PhaseFailed && prev.Restored
	if ws.ArchivedAt != nil && !retry {
		return nil, fmt.Errorf("%w: %s", ErrArchived, nameOf(ws.MovedTo))
	}
	if !retry {
		if err := s.idle(ctx, ws.ID); err != nil {
			return nil, err
		}
	}
	if _, err := s.opts.Dial(ctx, m.ID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.active[ws.ID] {
		s.mu.Unlock()
		return nil, ErrInFlight
	}
	s.active[ws.ID] = true
	s.mu.Unlock()

	mv := prev
	if mv == nil || mv.Phase == PhaseDone {
		now := time.Now().UTC()
		mv = &Move{ID: "mv_" + randHex(8), WorkspaceID: ws.ID, MachineID: m.ID, Phase: PhaseArchive, CreatedAt: now, UpdatedAt: now}
		if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO workspace_moves (id, workspace_id, machine_id, phase, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, mv.ID, mv.WorkspaceID, mv.MachineID, mv.Phase, now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
			s.release(ws.ID)
			return nil, err
		}
	}
	mv.Machine = m.Name
	mv.Error = ""
	if retry {
		s.phase(ctx, mv, PhaseCloud, cloudMsg(m.Name)) // before the 202, so that a poll does not read the old failure
	}
	s.wg.Add(1)
	go func() { // stops on Close (s.ctx) or at the end of the move
		defer s.wg.Done()
		s.run(s.ctx, ws, m, *mv) // the last phase releases the workspace
	}()
	return mv, nil
}

// Get returns one move of a workspace.
func (s *Service) Get(ctx context.Context, wsID, id string) (*Move, error) {
	return s.scan(s.st.DB().QueryRowContext(ctx, moveSelect+` WHERE w.workspace_id = ? AND w.id = ?`, wsID, id))
}

// Import restores an archive that another machine sends. On a failure it
// removes what it wrote.
func (s *Service) Import(ctx context.Context, r io.Reader) (*ImportResult, error) {
	u := &unpacker{st: s.st, ws: s.ws, home: s.opts.Home, secrets: s.opts.Secrets}
	fail := ""
	if s.opts.FailRestore {
		fail = "rows"
	}
	res, err := u.restore(ctx, r, fail)
	if err != nil {
		s.log.Warn("workspace.move", "phase", PhaseRestore, "err", err)
		return nil, err
	}
	s.log.Info("workspace.created", "workspace_id", res.WorkspaceID, "path", res.Path, "already", res.Already)
	return res, nil
}

const moveSelect = `SELECT w.id, w.workspace_id, w.machine_id, COALESCE(m.name, ''), w.phase, w.error, w.restored, w.created_at, w.updated_at
	FROM workspace_moves w LEFT JOIN machines m ON m.id = w.machine_id`

func (s *Service) scan(row *sql.Row) (*Move, error) {
	var mv Move
	var restored int
	var created, updated string
	if err := row.Scan(&mv.ID, &mv.WorkspaceID, &mv.MachineID, &mv.Machine, &mv.Phase, &mv.Error, &restored, &created, &updated); err != nil {
		return nil, err
	}
	mv.Restored = restored == 1
	mv.CreatedAt, _ = time.Parse(time.RFC3339, created)
	mv.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &mv, nil
}

func (s *Service) latest(ctx context.Context, wsID, machineID string) (*Move, error) {
	mv, err := s.scan(s.st.DB().QueryRowContext(ctx, moveSelect+` WHERE w.workspace_id = ? AND w.machine_id = ?
		ORDER BY w.created_at DESC, w.id DESC LIMIT 1`, wsID, machineID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return mv, err
}

func (s *Service) release(wsID string) {
	s.mu.Lock()
	delete(s.active, wsID)
	s.mu.Unlock()
}

func (s *Service) machine(ctx context.Context, ref string) (machines.Machine, error) {
	m, err := s.opts.Machines.Get(ctx, ref)
	if !errors.Is(err, machines.ErrNotFound) {
		return m, err
	}
	list, lerr := s.opts.Machines.List(ctx)
	if lerr != nil {
		return m, lerr
	}
	for _, c := range list {
		if strings.EqualFold(c.Name, ref) {
			return c, nil
		}
	}
	return m, err
}

// idle refuses a workspace with a live run, a live assistant turn, or a
// queued event.
func (s *Service) idle(ctx context.Context, wsID string) error {
	var n int
	err := s.st.DB().QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM runs WHERE workspace_id = ? AND status = 'running') +
		(SELECT COUNT(*) FROM event_queue WHERE workspace_id = ? AND status IN ('pending', 'claimed', 'blocked')) +
		(SELECT COUNT(*) FROM assistant_sessions WHERE workspace_id = ? AND status IN ('starting', 'running'))`,
		wsID, wsID, wsID).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrBusy
	}
	return nil
}

// run does the phases of one move. A failure before the restore leaves
// the source as it was.
func (s *Service) run(ctx context.Context, ws *workspaces.Workspace, m machines.Machine, mv Move) {
	log := s.log.With("move_id", mv.ID, "workspace_id", ws.ID, "machine_id", m.ID)
	fail := func(msg string, err error) {
		if err != nil {
			msg += ": " + err.Error()
		}
		log.Warn("workspace.move", "phase", PhaseFailed, "err", msg)
		s.phase(context.WithoutCancel(ctx), &mv, PhaseFailed, msg)
	}
	if !mv.Restored {
		res, err := s.send(ctx, ws, m, &mv)
		if err != nil {
			fail("The workspace did not move to "+m.Name+". It stays on this machine", err)
			return
		}
		mv.Restored = true
		s.phase(ctx, &mv, PhaseRestore, fmt.Sprintf("%s restored the workspace at %s: %d files, %d secrets added, %d secrets kept.",
			m.Name, res.Path, res.Files, len(res.SecretsAdded), len(res.SecretsKept)))
		if err := s.archive(ctx, ws.ID, m.ID, mv.ID); err != nil {
			fail("The workspace is on "+m.Name+", but this machine did not archive its copy", err)
			return
		}
	}
	if mv.Phase != PhaseCloud {
		s.phase(ctx, &mv, PhaseCloud, cloudMsg(m.Name))
	}
	if err := s.moveCloud(ctx, ws.ID, *m.DeviceID); err != nil {
		fail("The workspace is on "+m.Name+", but its mail address and webhooks still go to this machine. Try the move again", err)
		return
	}
	s.phase(ctx, &mv, PhaseDone, "The workspace moved to "+m.Name+". Log in to its connectors on "+m.Name+" again if they ask.")
	log.Info("workspace.move", "phase", PhaseDone)
}

// send streams the archive to the target and returns its restore result.
func (s *Service) send(ctx context.Context, ws *workspaces.Workspace, m machines.Machine, mv *Move) (*ImportResult, error) {
	s.phase(ctx, mv, PhaseArchive, "Packing the workspace.")
	rt, err := s.opts.Dial(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	self, err := s.opts.Machines.Self(ctx)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	p := packer{st: s.st, home: s.opts.Home, secrets: s.opts.Secrets}
	man := Manifest{
		MoveID:    mv.ID,
		Workspace: WorkspaceInfo{ID: ws.ID, Name: ws.Name, Handle: ws.Handle, Path: ws.Path},
		Source:    MachineInfo{ID: self.ID, Name: self.Name},
		CreatedAt: time.Now().UTC(),
	}
	done := make(chan error, 1)
	go func() { // ends when the archive is written or the pipe closes
		_, err := p.pack(ctx, pw, man, func() {
			s.phase(ctx, mv, PhaseSend, "Sending the workspace to "+m.Name+".")
		})
		_ = pw.CloseWithError(err) // the reader sees err, or EOF on nil
		done <- err
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer/v1/workspaces/import", pr)
	if err != nil {
		_ = pr.CloseWithError(err) // stops the packer
		<-done
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := (&http.Client{Transport: rt}).Do(req)
	_ = pr.CloseWithError(errors.New("request ended")) // stops the packer if the target stopped early
	packErr := <-done
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // an empty body gives the status error below
	if resp.StatusCode != http.StatusOK {
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
			return nil, errors.New(env.Error.Message)
		}
		return nil, fmt.Errorf("%s answered %d", m.Name, resp.StatusCode)
	}
	if packErr != nil {
		return nil, packErr
	}
	var res ImportResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// archive makes the source copy read-only once the target holds the
// workspace, so only one copy runs.
func (s *Service) archive(ctx context.Context, wsID, machineID, moveID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		var rev int64
		if err := tx.QueryRowContext(ctx, `UPDATE workspaces SET archived_at = ?, moved_to = ?, move_id = ?, revision = revision + 1, updated_at = ?
			WHERE id = ? RETURNING revision`, now, machineID, moveID, now, wsID).Scan(&rev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schedules SET next_due_at = NULL WHERE workspace_id = ?`, wsID); err != nil {
			return err
		}
		_, err := store.AppendInTx(tx, store.BusEvent{
			Topic: "workspace.updated", Entity: "workspace", EntityID: wsID, Action: "updated", Revision: rev,
			Attrs: map[string]string{"workspace": wsID, "archived": "true"},
		})
		return err
	})
	if err == nil {
		s.st.NotifyBus()
	}
	return err
}

func (s *Service) moveCloud(ctx context.Context, wsID, deviceID string) error {
	if s.opts.Cloud == nil {
		return errors.New("this machine is not linked to the cloud")
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id FROM cloud_hooks WHERE workspace_id = ? ORDER BY id`, wsID)
	if err != nil {
		return err
	}
	hooks := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		hooks = append(hooks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.opts.Cloud.MoveWorkspace(ctx, wsID, cloudlink.WorkspaceMoveRequest{DeviceID: deviceID, Hooks: hooks})
	return err
}

// phase stores the phase of a move and puts it on the bus.
func (s *Service) phase(ctx context.Context, mv *Move, phase, msg string) {
	mv.Phase = phase
	if phase == PhaseFailed {
		mv.Error = msg
	}
	now := time.Now().UTC().Format(time.RFC3339)
	restored := 0
	if mv.Restored {
		restored = 1
	}
	if phase == PhaseDone || phase == PhaseFailed {
		// Under the lock, so that a poll that reads the last phase can start the next move.
		s.mu.Lock()
		defer s.mu.Unlock()
		defer delete(s.active, mv.WorkspaceID)
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspace_moves SET phase = ?, error = ?, restored = ?, updated_at = ? WHERE id = ?`,
			phase, mv.Error, restored, now, mv.ID)
		return err
	}, store.BusEvent{
		Topic: "workspace.move", Entity: "workspace", EntityID: mv.WorkspaceID, Action: phase,
		Attrs: map[string]string{"workspace": mv.WorkspaceID, "move_id": mv.ID, "machine": mv.Machine, "phase": phase, "message": msg},
	})
	if err != nil {
		s.log.Warn("workspace.move", "move_id", mv.ID, "phase", phase, "err", err)
	}
}

func cloudMsg(machine string) string {
	return "Moving the mail address and webhooks to " + machine + "."
}

func nameOf(m *workspaces.MachineRef) string {
	if m == nil {
		return "another machine"
	}
	return m.Name
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand does not fail
	return hex.EncodeToString(b)
}
