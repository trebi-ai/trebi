package api

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// agentScope is the write guardrail for one kind=agent run (2026-09-24 F).
type agentScope struct {
	workspaceID string
	runID       string
	agent       string // agents/<id>
}

// scopeFrom returns the scope of an agent caller. Jobs, the assistant, and
// humans are not scoped by this guardrail.
func scopeFrom(ctx context.Context) (agentScope, bool) {
	a, ok := tools.ActorFrom(ctx)
	if !ok || a.Kind != "agent" || a.Job == "" {
		return agentScope{}, false
	}
	short := strings.TrimPrefix(a.Job, "agents/")
	if short == a.Job {
		return agentScope{}, false
	}
	return agentScope{workspaceID: a.WorkspaceID, runID: a.RunID, agent: "agents/" + short}, true
}

func (a agentScope) canActOnRun(run runs.Run, owner string) bool {
	return run.Workspace == a.workspaceID && (run.ID == a.runID || run.Job == a.agent || owner == a.agent)
}

func (a agentScope) canWriteJob(row workspaces.JobRow) bool {
	return row.WorkspaceID == a.workspaceID && row.Owner == a.agent
}

func (a agentScope) canWritePath(workspaceID, rel string) bool {
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	return workspaceID == a.workspaceID && strings.HasPrefix(clean, "/"+a.agent+"/")
}

// protectedChange reports identity, trust, and spend keys changed by an
// agent. These keys are human-only even for the agent itself.
func (a agentScope) protectedChange(old, new *job.Job) []string {
	return old.ProtectedChange(new)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func budgetEqual(a, b *job.BudgetYAML) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// requireAgentJobScope rejects a mutation of an unowned job.
func (s *Server) requireAgentJobScope(w http.ResponseWriter, r *http.Request, row workspaces.JobRow) bool {
	scope, ok := scopeFrom(r.Context())
	if !ok || scope.canWriteJob(row) {
		return true
	}
	writeErr(w, http.StatusForbidden, "forbidden", "agent may change only jobs owned by "+scope.agent)
	return false
}

// requireAgentRunScope permits an agent to manage its own run, a run of one
// of its owned jobs, or another run it owns as an agent job.
func (s *Server) requireAgentRunScope(w http.ResponseWriter, r *http.Request, runID string) bool {
	scope, ok := scopeFrom(r.Context())
	if !ok {
		return true
	}
	run, err := s.runs.Get(r.Context(), runID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return false
	}
	var owner string
	if row, err := s.findJobRow(r.Context(), run.Workspace, run.Job); err == nil {
		owner = row.Owner
	}
	if scope.canActOnRun(*run, owner) {
		return true
	}
	writeForbidden(w, "agent may change only its own runs")
	return false
}

func (s *Server) requireAgentQueueScope(w http.ResponseWriter, r *http.Request, id string) bool {
	scope, ok := scopeFrom(r.Context())
	if !ok {
		return true
	}
	var wsID, job string
	err := s.st.DB().QueryRowContext(r.Context(), `SELECT workspace_id, job FROM event_queue WHERE id=?`, id).
		Scan(&wsID, &job)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "queue item not found")
		return false
	}
	row, err := s.findJobRow(r.Context(), wsID, job)
	if err != nil || !scope.canWriteJob(row) {
		writeForbidden(w, "agent may change only queue items for jobs it owns")
		return false
	}
	return true
}

func (s *Server) requireAgentMemoryScope(w http.ResponseWriter, r *http.Request, target *memoryTarget) bool {
	scope, ok := scopeFrom(r.Context())
	if !ok || scope.canWritePath(target.ws.ID, target.rel) {
		return true
	}
	writeForbidden(w, "agent may write only its own ledger and owned jobs' ledgers")
	return false
}

func writeForbidden(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusForbidden, "forbidden", msg)
}

// assistantScoped rejects an assistant session actor whose token belongs to
// another workspace. Humans and agents pass; the handler owns the rest.
func (s *Server) assistantScoped(w http.ResponseWriter, r *http.Request, workspaceID string) bool {
	a, ok := tools.ActorFrom(r.Context())
	if !ok || a.Kind != "assistant" {
		return true
	}
	if a.WorkspaceID == workspaceID {
		return true
	}
	writeForbidden(w, "assistant may act only on its own workspace")
	return false
}

func rejectAnyAgentScope(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := scopeFrom(r.Context()); ok {
		writeForbidden(w, "agent callers may not change this resource")
		return true
	}
	return false
}
