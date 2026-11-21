package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// archivedView is the archived= filter of the list routes.
type archivedView string

const (
	viewCurrent  archivedView = "false"
	viewArchived archivedView = "true"
	viewAll      archivedView = "all"
)

const errArchivedValue = "archived must be false, true, or all"

// parseArchivedView reads the archived query value. An empty value is false.
func parseArchivedView(v string) (archivedView, bool) {
	switch archivedView(v) {
	case "", viewCurrent:
		return viewCurrent, true
	case viewArchived, viewAll:
		return archivedView(v), true
	}
	return "", false
}

func (v archivedView) keep(archived bool) bool {
	switch v {
	case viewAll:
		return true
	case viewArchived:
		return archived
	}
	return !archived
}

// archiveCounts is the counts block of a list reply. It counts before the
// archived filter.
type archiveCounts struct {
	Current  int `json:"current"`
	Archived int `json:"archived"`
}

func (c *archiveCounts) add(archived bool) {
	if archived {
		c.Archived++
	} else {
		c.Current++
	}
}

func (s *Server) handleJobArchive(w http.ResponseWriter, r *http.Request) {
	s.setJobArchived(w, r, true)
}

func (s *Server) handleJobUnarchive(w http.ResponseWriter, r *http.Request) {
	s.setJobArchived(w, r, false)
}

const ownerArchivedText = "The agent %s is archived. Unarchive the agent, or move the job to another agent."

func (s *Server) setJobArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	if !s.requireAgentJobScope(w, r, row) {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	res, ok := s.applyArchive(w, r, ws, row, archived)
	if !ok {
		return
	}
	out := map[string]any{"name": row.Name, "archived_at": res.archivedAt}
	if archived {
		out["queue_dropped"] = res.dropped
	}
	writeJSON(w, http.StatusOK, out)
}

type archiveOutcome struct {
	archivedAt string
	jobs       []string // names of the owned jobs of an agent
	dropped    int
}

// applyArchive writes the flag, refreshes the schedules, and on archive drops
// the queue rows of the object and of the jobs that it covers.
func (s *Server) applyArchive(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, row workspaces.JobRow, archived bool) (archiveOutcome, bool) {
	ctx := r.Context()
	res, err := s.ws.SetArchived(ctx, ws, row, archived, time.Now().UTC())
	if err != nil {
		if errors.Is(err, workspaces.ErrOwnerArchived) {
			writeErr(w, http.StatusConflict, "archived", fmt.Sprintf(ownerArchivedText, row.OwnerName))
			return archiveOutcome{}, false
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return archiveOutcome{}, false
	}
	s.recomputeMoved(ctx, ws.ID, res.Jobs)
	out := archiveOutcome{archivedAt: res.ArchivedAt}
	for _, j := range res.Jobs {
		if j.ID != row.ID {
			out.jobs = append(out.jobs, j.Name)
		}
	}
	if archived {
		ids := res.JobIDs
		if row.Kind == "agent" {
			ids = append(ids, row.ID)
		}
		out.dropped = s.dropArchivedQueue(ctx, ids)
	}
	return out, true
}

// dropArchivedQueue skips the waiting queue rows of the jobs. A hop row fails
// its hop, so the caller of an ask gets a reply.
func (s *Server) dropArchivedQueue(ctx context.Context, ids []string) int {
	dropped, err := s.queue.DropForJobs(ctx, ids, "archived")
	if err != nil {
		s.log.Warn("queue drop after archive failed", "error", err)
	}
	for _, it := range dropped {
		if it.HopID == "" {
			continue
		}
		if err := s.chains.LaunchFailed(ctx, it.HopID, runs.ErrJobArchived); err != nil {
			s.log.Warn("hop fail after archive failed", "hop", it.HopID, "error", err)
		}
	}
	return len(dropped)
}

func (s *Server) handleAgentArchive(w http.ResponseWriter, r *http.Request) {
	s.setAgentArchived(w, r, true)
}

func (s *Server) handleAgentUnarchive(w http.ResponseWriter, r *http.Request) {
	s.setAgentArchived(w, r, false)
}

// setAgentArchived archives an agent and, through it, its jobs. Only a person
// or the assistant calls it: an agent run token gets 403.
func (s *Server) setAgentArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.pathWorkspace(w, r)
	if !ok {
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	rel, err := workspaceRel(ws.Path, row.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	res, ok := s.applyArchive(w, r, ws, row, archived)
	if !ok {
		return
	}
	jobs := res.jobs
	if jobs == nil {
		jobs = []string{}
	}
	out := map[string]any{
		"name":        row.Name,
		"archived_at": res.archivedAt, "jobs": jobs,
	}
	if archived {
		out["queue_dropped"] = res.dropped
	}
	writeJSON(w, http.StatusOK, out)
}

// apply counts the agents before the archived filter and keeps the agents
// that the view shows.
func (v archivedView) apply(list []agentPayload) ([]agentPayload, archiveCounts) {
	var counts archiveCounts
	out := list[:0:0]
	for _, p := range list {
		archived := p.ArchivedAt != ""
		counts.add(archived)
		if v.keep(archived) {
			out = append(out, p)
		}
	}
	return out, counts
}

// archivedLaunchErr is the launch refusal of an archived row, or nil.
func archivedLaunchErr(row workspaces.JobRow) error {
	switch {
	case row.ArchivedAt != "":
		return runs.ArchivedErr(row.Kind == "agent", "")
	case row.OwnerArchivedAt != "":
		return runs.ArchivedErr(false, row.OwnerName)
	}
	return nil
}

// archivedSkillNotes lists the archived skills that a job asks for. The job
// page shows them, because a run does not use them.
func (s *Server) archivedSkillNotes(ctx context.Context, rs skills.RunScope, names []string) []map[string]string {
	out := []map[string]string{}
	if s.skills == nil {
		return out
	}
	list, err := s.skills.ArchivedFor(ctx, rs, names)
	if err != nil {
		s.log.Warn("archived skills lookup failed", "error", err)
		return out
	}
	for _, sk := range list {
		out = append(out, map[string]string{"name": sk.Name, "ref": sk.Ref, "scope": string(sk.Scope)})
	}
	return out
}

func (s *Server) handleSkillArchive(w http.ResponseWriter, r *http.Request) {
	s.setSkillArchived(w, r, true)
}

func (s *Server) handleSkillUnarchive(w http.ResponseWriter, r *http.Request) {
	s.setSkillArchived(w, r, false)
}

func (s *Server) setSkillArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	ref, ok := s.skillRef(w, r, true)
	if !ok || !s.requireSkillWrite(w, r, ref) {
		return
	}
	var sk *skills.Skill
	var err error
	if archived {
		sk, err = s.skills.Archive(r.Context(), ref, r.Header.Get("If-Match"), time.Now())
	} else {
		sk, err = s.skills.Unarchive(r.Context(), ref, r.Header.Get("If-Match"))
	}
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}
