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
	agentID     string
	agent       string // <name>
	dir         string // agents/<name>
}

// scopeFrom returns the scope of an agent caller. Jobs, the assistant, and
// humans are not scoped by this guardrail.
func scopeFrom(ctx context.Context) (agentScope, bool) {
	a, ok := tools.ActorFrom(ctx)
	if !ok || a.Kind != "agent" || !job.IsAgentName(a.Job) {
		return agentScope{}, false
	}
	return agentScope{workspaceID: a.WorkspaceID, runID: a.RunID, agentID: a.JobID, agent: a.Job, dir: job.Dir(a.Job)}, true
}

func (a agentScope) canActOnRun(run runs.Run, ownerID string) bool {
	return run.Workspace == a.workspaceID && (run.ID == a.runID || run.JobID == a.agentID || ownerID == a.agentID)
}

func (a agentScope) canWriteJob(row workspaces.JobRow) bool {
	return row.WorkspaceID == a.workspaceID && row.OwnerID == a.agentID
}

func (a agentScope) canWritePath(workspaceID, rel string) bool {
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	return workspaceID == a.workspaceID && strings.HasPrefix(clean, "/"+a.dir+"/")
}

// requireAgentJobScope rejects a mutation of a job of another agent.
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
	var ownerID string
	if row, err := s.ws.JobByID(r.Context(), run.JobID); err == nil {
		ownerID = row.OwnerID
	}
	if scope.canActOnRun(*run, ownerID) {
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
	var jobID string
	err := s.st.DB().QueryRowContext(r.Context(), `SELECT job_id FROM event_queue WHERE id=?`, id).Scan(&jobID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "queue item not found")
		return false
	}
	row, err := s.ws.JobByID(r.Context(), jobID)
	if err != nil || !scope.canWriteJob(*row) {
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

// rejectAnyRunToken refuses a job or agent run. The assistant and humans pass.
func rejectAnyRunToken(w http.ResponseWriter, r *http.Request) bool {
	if a, ok := tools.ActorFrom(r.Context()); ok && a.RunID != "" && a.Kind != "assistant" {
		writeForbidden(w, "a job run cannot answer a request")
		return true
	}
	return false
}
