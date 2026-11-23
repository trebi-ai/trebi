package chains

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

const chainCols = `trace_id, workspace_id, root_kind, root_run_id, root_conversation_id, root_agent_id,
	status, max_hops, max_turns, max_usd, stopped_by, stop_reason, created_at, updated_at, revision`

func scanChain(row scanner) (*Chain, error) {
	var c Chain
	var created, updated string
	err := row.Scan(&c.TraceID, &c.Workspace, &c.RootKind, &c.RootRunID, &c.RootConversationID, &c.rootAgentID,
		&c.Status, &c.Limits.MaxHops, &c.Limits.MaxTurns, &c.Limits.MaxUSD, &c.StoppedBy, &c.StopReason,
		&created, &updated, &c.Revision)
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = store.ParseTS(created), store.ParseTS(updated)
	return &c, nil
}

// chain reads one chain row with no usage.
func (s *Service) chain(ctx context.Context, traceID string) (*Chain, error) {
	c, err := scanChain(s.st.DB().QueryRowContext(ctx, `SELECT `+chainCols+` FROM chains WHERE trace_id=?`, traceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("chain %s: %w", traceID, ErrNotFound)
	}
	return c, err
}

// fill sets the usage, the live flag, and the root agent name.
func (s *Service) fill(ctx context.Context, c *Chain) error {
	used, err := s.runs.TraceUsage(ctx, c.TraceID)
	if err != nil {
		return err
	}
	c.Usage.Turns, c.Usage.CostUSD, c.Usage.Tokens = used.Turns, used.CostUSD, used.Tokens
	var live int
	err = s.st.DB().QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(depth) FROM chain_hops WHERE trace_id=? AND status NOT IN ('refused')), 0),
		(SELECT COUNT(DISTINCT run_id) FROM run_turns WHERE trace_id=?),
		(SELECT COUNT(*) FROM chain_hops WHERE trace_id=? AND status IN ('queued','running'))`,
		c.TraceID, c.TraceID, c.TraceID).Scan(&c.Usage.Depth, &c.Usage.Runs, &live)
	if err != nil {
		return err
	}
	c.Live = live > 0
	if c.rootAgentID != "" {
		c.RootAgent = s.agentNames(ctx, c.rootAgentID)[c.rootAgentID]
	}
	return nil
}

// Get returns a chain and its hops, oldest first.
func (s *Service) Get(ctx context.Context, traceID string) (*Chain, []Hop, error) {
	c, err := s.chain(ctx, traceID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.fill(ctx, c); err != nil {
		return nil, nil, err
	}
	hops, err := s.queryHops(ctx, `WHERE trace_id=? ORDER BY created_at, id`, traceID)
	if err != nil {
		return nil, nil, err
	}
	s.decorate(ctx, c.RootKind, hops...)
	out := make([]Hop, len(hops))
	for i, h := range hops {
		h.Text, h.Context = "", ""
		out[i] = *h
	}
	return c, out, nil
}

// ListFilter selects chains for List.
type ListFilter struct {
	Workspace string
	Status    string
	Agent     string // agent ID or name (cmo)
	Live      bool
	Since     time.Time
	Limit     int
	Cursor    string
}

// List returns chains newest first and the cursor of the next page.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Chain, string, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where, args := f.where()
	q := `SELECT ` + chainCols + ` FROM chains WHERE 1=1` + where
	q += ` ORDER BY trace_id DESC LIMIT ?`
	args = append(args, f.Limit+1)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	var list []*Chain
	for rows.Next() {
		c, err := scanChain(rows)
		if err != nil {
			rows.Close()
			return nil, "", err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > f.Limit {
		list = list[:f.Limit]
		next = list[len(list)-1].TraceID
	}
	out := make([]Chain, 0, len(list))
	for _, c := range list {
		if err := s.fill(ctx, c); err != nil {
			return nil, "", err
		}
		out = append(out, *c)
	}
	return out, next, nil
}

// where returns the filter clauses of f and their args.
func (f ListFilter) where() (string, []any) {
	var q string
	var args []any
	if f.Workspace != "" {
		q += ` AND workspace_id=?`
		args = append(args, f.Workspace)
	}
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.Agent != "" {
		q += ` AND EXISTS (SELECT 1 FROM chain_hops h WHERE h.trace_id=chains.trace_id AND (h.from_agent_id IN
			(SELECT id FROM job_index WHERE id=? OR name=?) OR h.to_agent_id IN (SELECT id FROM job_index WHERE id=? OR name=?)))`
		args = append(args, f.Agent, f.Agent, f.Agent, f.Agent)
	}
	if f.Live {
		q += ` AND EXISTS (SELECT 1 FROM chain_hops h WHERE h.trace_id=chains.trace_id AND h.status IN ('queued','running'))`
	}
	if !f.Since.IsZero() {
		q += ` AND updated_at>=?`
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if f.Cursor != "" {
		q += ` AND trace_id<?`
		args = append(args, f.Cursor)
	}
	return q, args
}

// Hop returns one hop with its text and context.
func (s *Service) Hop(ctx context.Context, id string) (*Hop, error) {
	h, err := s.hop(ctx, id)
	if err != nil {
		return nil, err
	}
	rootKind := EndHuman
	if c, err := s.chain(ctx, h.TraceID); err == nil {
		rootKind = c.RootKind
	}
	s.decorate(ctx, rootKind, h)
	return h, nil
}

// decorate sets the agent names. A reply with no target agent goes to the
// chain root, so its end kind is the root kind.
func (s *Service) decorate(ctx context.Context, rootKind string, hops ...*Hop) {
	var ids []string
	for _, h := range hops {
		ids = append(ids, h.pathIDs...)
		ids = append(ids, h.From.AgentID, h.To.AgentID)
	}
	n := s.agentNames(ctx, ids...)
	for _, h := range hops {
		h.Path = make([]string, len(h.pathIDs))
		for i, id := range h.pathIDs {
			h.Path[i] = nameOr(n, id)
		}
		h.From.Agent, h.To.Agent = nameOr(n, h.From.AgentID), nameOr(n, h.To.AgentID)
		if h.To.AgentID == "" {
			h.To.Kind = rootKind
		}
	}
}

func nameOr(n map[string]string, id string) string {
	if id == "" {
		return ""
	}
	if name, ok := n[id]; ok {
		return name
	}
	return id
}

// agentNames maps agent IDs to short names from the job index.
func (s *Service) agentNames(ctx context.Context, ids ...string) map[string]string {
	out := map[string]string{}
	var args []any
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			args = append(args, id)
		}
	}
	if len(args) == 0 {
		return out
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, name FROM job_index WHERE id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}

// ForRun returns the chain block of a run, or nil when no turn of the run
// is in a chain.
func (s *Service) ForRun(ctx context.Context, runID string) (*RunChain, error) {
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	var traceID, path string
	var depth int
	err = s.st.DB().QueryRowContext(ctx, `SELECT t.trace_id, t.depth, t.path_json FROM run_turns t
		JOIN chains c ON c.trace_id = t.trace_id WHERE t.run_id=? ORDER BY t.n DESC LIMIT 1`, runID).Scan(&traceID, &depth, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := s.chain(ctx, traceID)
	if err != nil {
		return nil, err
	}
	if err := s.fill(ctx, c); err != nil {
		return nil, err
	}
	in, err := s.queryHops(ctx, `WHERE to_run_id=? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	out, err := s.queryHops(ctx, `WHERE from_run_id=? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	s.decorate(ctx, c.RootKind, append(append([]*Hop{}, in...), out...)...)
	rc := &RunChain{TraceID: traceID, Status: c.Status, Depth: depth, Limits: c.Limits, Usage: c.Usage,
		In: refs(in), Out: refs(out)}
	var ids []string
	_ = json.Unmarshal([]byte(path), &ids) // a bad row reads as no path
	n := s.agentNames(ctx, ids...)
	rc.Path = make([]string, len(ids))
	for i, id := range ids {
		rc.Path[i] = nameOr(n, id)
	}
	if len(rc.Path) == 0 && run.JobID != "" {
		rc.Path = []string{nameOr(s.agentNames(ctx, run.JobID), run.JobID)}
	}
	return rc, nil
}

// Runs returns the runs of a chain with the turns of the trace, oldest first.
func (s *Service) Runs(ctx context.Context, traceID string) ([]ChainRun, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT run_id, n, depth FROM run_turns WHERE trace_id=? ORDER BY started_at, run_id, n`, traceID)
	if err != nil {
		return nil, err
	}
	var order []string
	byRun := map[string]*ChainRun{}
	for rows.Next() {
		var id string
		var n, depth int
		if err := rows.Scan(&id, &n, &depth); err != nil {
			rows.Close()
			return nil, err
		}
		cr, ok := byRun[id]
		if !ok {
			cr = &ChainRun{Depth: depth}
			byRun[id] = cr
			order = append(order, id)
		}
		cr.Turns = append(cr.Turns, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ChainRun, 0, len(order))
	for _, id := range order {
		run, err := s.runs.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("chain run %s: %w", id, err)
		}
		cr := byRun[id]
		cr.RunSummary = runs.RunSummary{ID: run.ID, Status: run.Status, EndReason: run.EndReason,
			StartedAt: run.StartedAt, DurationMS: run.DurationMS, CostUSD: run.Tokens.CostUSD, Tokens: run.Tokens}
		cr.Agent, cr.Kind = run.JobName, run.Kind
		out = append(out, *cr)
	}
	return out, nil
}

func refs(hops []*Hop) []HopRef {
	out := make([]HopRef, 0, len(hops))
	for _, h := range hops {
		out = append(out, h.Ref())
	}
	return out
}

// TurnHops returns the hops that started turns of a run, keyed by hop ID.
func (s *Service) TurnHops(ctx context.Context, runID string) (map[string]HopRef, error) {
	hops, err := s.queryHops(ctx, `WHERE id IN (SELECT hop_id FROM run_turns WHERE run_id=? AND hop_id<>'')`, runID)
	if err != nil {
		return nil, err
	}
	rootKind := EndHuman
	if len(hops) > 0 {
		if c, err := s.chain(ctx, hops[0].TraceID); err == nil {
			rootKind = c.RootKind
		}
	}
	s.decorate(ctx, rootKind, hops...)
	out := make(map[string]HopRef, len(hops))
	for _, h := range hops {
		out[h.ID] = h.Ref()
	}
	return out, nil
}

// AgentNames maps agent IDs to short names.
func (s *Service) AgentNames(ctx context.Context, ids []string) []string {
	n := s.agentNames(ctx, ids...)
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = nameOr(n, id)
	}
	return out
}

// MaxWait caps the wait of an ask for its reply.
const MaxWait = 120 * time.Second

// WaitReplies waits up to wait for the replies of an ask, with their text.
func (s *Service) WaitReplies(ctx context.Context, askID string, wait time.Duration) ([]Hop, error) {
	ctx, cancel := context.WithTimeout(ctx, min(wait, MaxWait))
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		hops, err := s.queryHops(ctx, `WHERE reply_of=? AND kind=? ORDER BY created_at, id`, askID, string(KindReply))
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
		if len(hops) > 0 || ctx.Err() != nil {
			rootKind := EndHuman
			if len(hops) > 0 {
				if c, err := s.chain(context.WithoutCancel(ctx), hops[0].TraceID); err == nil {
					rootKind = c.RootKind
				}
			}
			s.decorate(context.WithoutCancel(ctx), rootKind, hops...)
			out := make([]Hop, len(hops))
			for i, h := range hops {
				out[i] = *h
			}
			return out, nil
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}
