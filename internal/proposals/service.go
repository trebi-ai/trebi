// Package proposals persists and decides human-approved AGENT.md changes.
package proposals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

const maxProposalBytes = 256 << 10

type Status string

const (
	StatusPending    Status = "pending"
	StatusApplied    Status = "applied"
	StatusDenied     Status = "denied"
	StatusStale      Status = "stale"
	StatusSuperseded Status = "superseded"
)

var (
	ErrNotPending = errors.New("proposal is not pending")
	ErrStale      = errors.New("AGENT.md changed after the proposal")
	ErrInvalid    = errors.New("proposed AGENT.md does not parse")
	ErrTooLarge   = errors.New("proposed AGENT.md is too large")
	ErrNoChange   = errors.New("proposed AGENT.md is unchanged")
)

type Proposal struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspace"`
	AgentID       string     `json:"agent_id"`
	AgentName     string     `json:"agent_name"`
	RunID         string     `json:"run_id,omitempty"`
	RelPath       string     `json:"path"`
	BaseHash      string     `json:"base_hash"`
	BaseContent   string     `json:"base_content"`
	Proposed      string     `json:"proposed"`
	Note          string     `json:"note,omitempty"`
	ProtectedKeys []string   `json:"protected_keys"`
	Status        Status     `json:"status"`
	DecidedBy     string     `json:"decided_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	Revision      int        `json:"revision"`
}

type fileWriter interface {
	Read(ctx context.Context, root, rel string) ([]byte, files.Info, error)
	Write(ctx context.Context, root, rel string, data []byte, ifMatch string) (files.Info, bool, error)
}

type indexer interface {
	Get(ctx context.Context, id string) (*workspaces.Workspace, error)
	RebuildIndex(ctx context.Context, ws *workspaces.Workspace) error
	JobByName(ctx context.Context, workspaceID, name string) (*workspaces.JobRow, error)
}

type bus interface {
	Append(ctx context.Context, ev store.BusEvent) (int64, error)
}

type runMessenger interface {
	SendLive(ctx context.Context, runID, text string) error
}

type Service struct {
	st    *store.Store
	files fileWriter
	ws    indexer
	bus   bus
	runs  runMessenger
	now   func() time.Time
}

func NewService(st *store.Store, f fileWriter, ws indexer, b bus, r runMessenger) *Service {
	return &Service{st: st, files: f, ws: ws, bus: b, runs: r, now: func() time.Time { return time.Now().UTC() }}
}

type CreateOpts struct {
	WorkspaceID string
	Agent       string
	RunID       string
	Proposed    []byte
	Note        string
}

// NoChange is returned when proposed bytes already equal the current file.
type NoChange struct{}

func (s *Service) Create(ctx context.Context, o CreateOpts) (Proposal, error) {
	if len(o.Proposed) > maxProposalBytes {
		return Proposal{}, ErrTooLarge
	}
	if !job.IsAgentName(o.Agent) {
		return Proposal{}, ErrInvalid
	}
	short := o.Agent
	rel := job.Dir(o.Agent) + "/AGENT.md"
	ws, err := s.ws.Get(ctx, o.WorkspaceID)
	if err != nil {
		return Proposal{}, err
	}
	base, _, err := s.files.Read(ctx, ws.Path, rel)
	if err != nil && !errors.Is(err, files.ErrNotFound) {
		return Proposal{}, err
	}
	row, err := s.ws.JobByName(ctx, o.WorkspaceID, o.Agent)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: agent %s not found", ErrInvalid, short)
	}
	if err != nil {
		return Proposal{}, err
	}
	// The proposal cannot change or drop the agent ID.
	if o.Proposed, err = job.WithID(o.Proposed, row.ID); err != nil {
		return Proposal{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	nextPath := path.Join(ws.Path, rel)
	next, parseErr := job.ParseBytes(nextPath, o.Proposed)
	if parseErr != nil {
		return Proposal{}, fmt.Errorf("%w: %s", ErrInvalid, parseErr)
	}
	next.Kind = "agent"
	if err := job.ValidateAgent(next, short); err != nil {
		return Proposal{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	var old *job.Job
	if len(base) > 0 {
		old, _ = job.ParseBytes(nextPath, base)
	}
	if old != nil {
		old.Kind = "agent"
	}
	if string(base) == string(o.Proposed) {
		return Proposal{}, ErrNoChange
	}
	added, removed := DiffStats(string(base), string(o.Proposed))
	protected := old.ProtectedChange(next)
	protectedJSON, _ := json.Marshal(protected)
	baseHash := job.ContentHash(base)
	p := Proposal{
		ID: "prop_" + strings.ToLower(ulid.Make().String()), WorkspaceID: o.WorkspaceID,
		AgentID: row.ID, AgentName: short, RunID: o.RunID, RelPath: rel, BaseHash: baseHash,
		BaseContent: string(base), Proposed: string(o.Proposed), Note: o.Note,
		ProtectedKeys: protected, Status: StatusPending, CreatedAt: s.now().UTC(), Revision: 1,
	}
	var superseded []Proposal
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, run_id FROM agent_proposals
WHERE workspace_id=? AND agent_id=? AND status=? AND id<>?`,
			p.WorkspaceID, p.AgentID, string(StatusPending), p.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var old Proposal
			if err := rows.Scan(&old.ID, &old.RunID); err != nil {
				rows.Close()
				return err
			}
			superseded = append(superseded, old)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, old := range superseded {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_proposals SET status=?, decided_at=?, revision=revision+1 WHERE id=?`,
				string(StatusSuperseded), p.CreatedAt.Format(time.RFC3339Nano), old.ID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_proposals
(id, workspace_id, agent_id, run_id, rel_path, base_hash, base_content, proposed,
  note, protected_keys, status, created_at, revision)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.WorkspaceID, p.AgentID, p.RunID, p.RelPath, p.BaseHash, p.BaseContent,
			p.Proposed, p.Note, string(protectedJSON), string(StatusPending),
			p.CreatedAt.Format(time.RFC3339Nano), p.Revision)
		return err
	})
	if err != nil {
		return Proposal{}, err
	}
	for _, old := range superseded {
		oldAttrs, _ := json.Marshal(map[string]any{"workspace": p.WorkspaceID, "agent_id": p.AgentID, "agent_name": p.AgentName, "proposal_id": old.ID, "decision": "superseded"})
		if _, err := s.bus.Append(ctx, store.BusEvent{Topic: "proposal.decided", Entity: "proposal", EntityID: old.ID, Action: "superseded", Attrs: oldAttrs}); err != nil {
			return Proposal{}, err
		}
	}
	attrs, _ := json.Marshal(map[string]any{
		"workspace": p.WorkspaceID, "agent_id": p.AgentID, "agent_name": p.AgentName, "proposal_id": p.ID,
		"run_id": p.RunID, "protected_keys": p.ProtectedKeys,
		"added": added, "removed": removed,
	})
	_, err = s.bus.Append(ctx, store.BusEvent{
		Topic: "proposal.created", Entity: "proposal", EntityID: p.ID, Action: "created", Attrs: attrs,
	})
	return p, err
}

type ListOpts struct {
	Workspace string
	Agent     string // ID or name
	State     string
	Limit     int
}

func (s *Service) List(ctx context.Context, o ListOpts) ([]Proposal, error) {
	limit := o.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := []string{"1=1"}
	args := []any{}
	if o.Workspace != "" {
		where = append(where, "workspace_id=?")
		args = append(args, o.Workspace)
	}
	if o.Agent != "" {
		where = append(where, `(agent_id=? OR agent_id IN (SELECT ji.id FROM job_index ji
  WHERE ji.workspace_id=agent_proposals.workspace_id AND ji.name=? AND ji.retired_at IS NULL))`)
		args = append(args, o.Agent, o.Agent)
	}
	switch o.State {
	case "pending", "decided":
		op := "="
		if o.State == "decided" {
			op = "<>"
		}
		where = append(where, "status "+op+" ?")
		args = append(args, string(StatusPending))
	case "":
	default:
		return nil, fmt.Errorf("state must be pending or decided")
	}
	args = append(args, limit)
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+proposalColumns+`
FROM agent_proposals WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var p Proposal
		if err := scanProposal(rows, &p, true); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, id string) (Proposal, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM agent_proposals WHERE id=?`, id)
	var p Proposal
	err := scanProposal(row, &p, false)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("proposal not found")
	}
	return p, err
}

type Decision struct {
	Allow bool
	By    string
}

func (s *Service) Decide(ctx context.Context, id string, d Decision) (Proposal, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	if p.Status != StatusPending {
		return p, ErrNotPending
	}
	now := s.now().UTC()
	p.DecidedBy, p.DecidedAt = d.By, &now
	if d.Allow {
		ws, err := s.ws.Get(ctx, p.WorkspaceID)
		if err != nil {
			return Proposal{}, err
		}
		target := filepath.Join(ws.Path, filepath.FromSlash(p.RelPath))
		prevArchived := ""
		if prev, _ := job.ParseFile(target); prev != nil {
			prevArchived = prev.ArchivedAt
		}
		_, _, err = s.files.Write(ctx, ws.Path, p.RelPath, []byte(p.Proposed), p.BaseHash)
		if err == nil {
			err = job.KeepArchived(target, prevArchived)
		}
		var stale *files.StaleError
		if errors.As(err, &stale) {
			p.Status = StatusStale
			if _, ferr := s.finishDecision(ctx, p); ferr != nil {
				return p, ferr
			}
			s.notifyRun(ctx, p, "is stale: AGENT.md changed. Read it again and propose again.")
			return p, ErrStale
		}
		if err != nil {
			return Proposal{}, err
		}
		p.Status = StatusApplied
		if _, err := s.finishDecision(ctx, p); err != nil {
			return Proposal{}, err
		}
		if err := s.ws.RebuildIndex(ctx, ws); err != nil {
			return Proposal{}, err
		}
		s.notifyRun(ctx, p, "was approved.")
		return p, nil
	}
	p.Status = StatusDenied
	if _, err := s.finishDecision(ctx, p); err != nil {
		return Proposal{}, err
	}
	s.notifyRun(ctx, p, "was denied.")
	return p, nil
}

func (s *Service) notifyRun(ctx context.Context, p Proposal, outcome string) {
	if p.RunID == "" || s.runs == nil {
		return
	}
	_ = s.runs.SendLive(ctx, p.RunID, "Your AGENT.md proposal "+p.ID+" "+outcome) // best effort: an ended run does not need the news
}

func (s *Service) finishDecision(ctx context.Context, p Proposal) (Proposal, error) {
	decidedAt := ""
	if p.DecidedAt != nil {
		decidedAt = p.DecidedAt.Format(time.RFC3339Nano)
	}
	_, err := s.st.DB().ExecContext(ctx, `UPDATE agent_proposals SET status=?, decided_by=?, decided_at=?, revision=revision+1 WHERE id=?`,
		string(p.Status), p.DecidedBy, decidedAt, p.ID)
	return p, err
}
