package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeSkills(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/skills", s.handleSkills)
	mux.HandleFunc("POST /v1/skills", s.handleSkillsCreate)
	mux.HandleFunc("GET /v1/skills/{name}", s.handleSkillGet)
	mux.HandleFunc("PUT /v1/skills/{name}", s.handleSkillPut)
	mux.HandleFunc("DELETE /v1/skills/{name}", s.handleSkillDelete)
	mux.HandleFunc("POST /v1/skills/{name}/update", s.handleSkillRefresh)
	mux.HandleFunc("POST /v1/skills/{name}/refresh", s.handleSkillRefresh)
	mux.HandleFunc("POST /v1/skills/{name}/move", s.handleSkillMove)
	mux.HandleFunc("POST /v1/skills/{name}/approve", s.handleSkillApprove)
	mux.HandleFunc("POST /v1/skills/{name}/archive", s.handleSkillArchive)
	mux.HandleFunc("POST /v1/skills/{name}/unarchive", s.handleSkillUnarchive)
}

// errSkillForbidden is a write outside the scopes of a run token.
var errSkillForbidden = errors.New("agents and jobs can change only their own skills; copy it to this agent to change it")

// skillOwnerID turns an owner name or ID into the owner ID of a scope.
// workspace names the workspace of an agent or job name.
func (s *Server) skillOwnerID(ctx context.Context, scope skills.Scope, owner, workspace string) (string, error) {
	switch scope {
	case skills.ScopeGlobal:
		if owner != "" {
			return "", fmt.Errorf("%w: a global skill has no owner", skills.ErrBadRef)
		}
		return "", nil
	case skills.ScopeWorkspace:
		ws, err := s.ws.Resolve(ctx, owner)
		if err != nil {
			return "", fmt.Errorf("%w: workspace %q", skills.ErrNoOwner, owner)
		}
		return ws.ID, nil
	case skills.ScopeAgent, skills.ScopeJob:
		if owner == "" {
			return "", fmt.Errorf("%w: a %s skill needs an owner", skills.ErrBadRef, scope)
		}
		find := s.findJobRow
		if scope == skills.ScopeAgent {
			find = s.findAgentRow
		}
		row, err := find(ctx, s.skillWorkspaceID(ctx, workspace), owner)
		if err != nil {
			return "", fmt.Errorf("%w: %s %q: %w", skills.ErrNoOwner, scope, owner, err)
		}
		return row.ID, nil
	}
	return "", fmt.Errorf("%w: scope must be global, workspace, agent, or job", skills.ErrBadRef)
}

// skillWorkspaceID is the workspace that finds an agent or job name: the
// workspace param, else the workspace of the calling run.
func (s *Server) skillWorkspaceID(ctx context.Context, workspace string) string {
	if workspace != "" {
		return s.workspaceID(ctx, workspace)
	}
	if a, ok := tools.ActorFrom(ctx); ok {
		return a.WorkspaceID
	}
	return ""
}

// skillRef reads the ref of {name} from ?scope=&owner=&workspace=. With no
// scope, the one skill of that name is used. When a name is in more than one
// scope, a read gets the global skill and a write gets 409 ambiguous.
func (s *Server) skillRef(w http.ResponseWriter, r *http.Request, write bool) (skills.Ref, bool) {
	ctx := r.Context()
	name := r.PathValue("name")
	q := r.URL.Query()
	if scope := skills.Scope(q.Get("scope")); scope != "" {
		id, err := s.skillOwnerID(ctx, scope, q.Get("owner"), q.Get("workspace"))
		if err != nil {
			s.writeSkillErr(w, err)
			return skills.Ref{}, false
		}
		ref := skills.Ref{Scope: scope, Owner: id, Name: name}
		if err := ref.Check(); err != nil {
			s.writeSkillErr(w, err)
			return skills.Ref{}, false
		}
		return ref, true
	}
	if err := skills.CheckName(name); err != nil {
		s.writeSkillErr(w, err)
		return skills.Ref{}, false
	}
	list, err := s.skills.Named(ctx, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return skills.Ref{}, false
	}
	switch {
	case len(list) == 0:
		return skills.GlobalRef(name), true
	case len(list) == 1 || (!write && list[0].Scope == skills.ScopeGlobal):
		ref, err := skills.ParseRef(list[0].Ref)
		if err != nil {
			s.writeSkillErr(w, err)
			return skills.Ref{}, false
		}
		return ref, true
	}
	refs := make([]string, len(list))
	for i, sk := range list {
		refs[i] = sk.Ref
	}
	writeErrDetails(w, http.StatusConflict, "ambiguous",
		"skill "+name+" is in more than one scope; set scope and owner", map[string]any{"refs": refs})
	return skills.Ref{}, false
}

// canWriteSkill applies D3 and D5. Humans write all scopes. The assistant
// writes global skills and the skills of its own workspace. An agent run
// writes its own agent scope and the scopes of jobs it owns. A job run
// writes its own job scope.
func (s *Server) canWriteSkill(ctx context.Context, ref skills.Ref) error {
	a, ok := tools.ActorFrom(ctx)
	if !ok {
		return nil
	}
	switch a.Kind {
	case "assistant":
		if ref.Scope == skills.ScopeGlobal || s.skillWorkspace(ctx, ref) == a.WorkspaceID {
			return nil
		}
		return errors.New("assistant may act only on its own workspace")
	case "agent":
		if ref.Scope == skills.ScopeAgent && ref.Owner == a.JobID {
			return s.skillPolicyAllows(ctx, a)
		}
		if ref.Scope == skills.ScopeJob {
			if row, err := s.ws.JobByID(ctx, ref.Owner); err == nil && row.OwnerID == a.JobID {
				return s.skillPolicyAllows(ctx, a)
			}
		}
		return errSkillForbidden
	case "job":
		if ref.Scope == skills.ScopeJob && ref.Owner == a.JobID {
			return s.skillPolicyAllows(ctx, a)
		}
		return errSkillForbidden
	}
	if a.RunID != "" {
		return errSkillForbidden
	}
	return nil
}

// skillPolicyAllows refuses all skill writes of a run whose agent kit sets
// skill_policy.create to false.
func (s *Server) skillPolicyAllows(ctx context.Context, a tools.Actor) error {
	if kit := s.ws.OwnerKit(ctx, a.WorkspaceID, a.Job); kit != nil && kit.SkillPolicy != nil && !kit.SkillPolicy.CanCreate() {
		return errors.New("the agent kit turns off skill changes (skill_policy.create is false)")
	}
	return nil
}

// skillWorkspace returns the workspace of a scoped ref, or "".
func (s *Server) skillWorkspace(ctx context.Context, ref skills.Ref) string {
	switch ref.Scope {
	case skills.ScopeWorkspace:
		return ref.Owner
	case skills.ScopeAgent, skills.ScopeJob:
		if row, err := s.ws.JobByID(ctx, ref.Owner); err == nil {
			return row.WorkspaceID
		}
	}
	return ""
}

func (s *Server) requireSkillWrite(w http.ResponseWriter, r *http.Request, ref skills.Ref) bool {
	if err := s.canWriteSkill(r.Context(), ref); err != nil {
		writeForbidden(w, err.Error())
		return false
	}
	return true
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if ref := q.Get("effective_for"); ref != "" {
		s.handleSkillsEffective(w, r, ref)
		return
	}
	view, ok := parseArchivedView(q.Get("archived"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "validation", errArchivedValue)
		return
	}
	opts := skills.ListOpts{Scope: skills.Scope(q.Get("scope")), Archived: skills.ArchivedAll}
	if opts.Scope != "" && !opts.Scope.Valid() {
		writeErr(w, http.StatusBadRequest, "validation", "scope must be global, workspace, agent, or job")
		return
	}
	if ws := q.Get("workspace"); ws != "" {
		row, err := s.ws.Resolve(ctx, ws)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		opts.WorkspaceID = row.ID
	}
	if owner := q.Get("owner"); owner != "" {
		if opts.Scope == "" || opts.Scope == skills.ScopeGlobal {
			writeErr(w, http.StatusBadRequest, "validation", "owner needs scope workspace, agent, or job")
			return
		}
		id, err := s.skillOwnerID(ctx, opts.Scope, owner, q.Get("workspace"))
		if err != nil {
			s.writeSkillErr(w, err)
			return
		}
		opts.OwnerID = id
	}
	all, err := s.skills.List(ctx, opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var counts archiveCounts
	list := []skills.Skill{}
	for _, sk := range all {
		counts.add(sk.ArchivedAt != "")
		if view.keep(sk.ArchivedAt != "") {
			list = append(list, sk)
		}
	}
	// Ref-keyed collection: limit only, no ULID cursor.
	limit := parseLimit(r)
	truncated := len(list) > limit
	if truncated {
		list = list[:limit]
	}
	resp := map[string]any{"skills": list, "counts": counts}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSkillsEffective returns the skills that a run of one job or agent
// gets, narrow first, with the overwritten ones marked.
func (s *Server) handleSkillsEffective(w http.ResponseWriter, r *http.Request, ref string) {
	ctx := r.Context()
	row, err := s.findIndexRow(ctx, s.skillWorkspaceID(ctx, r.URL.Query().Get("workspace")), ref, func(row workspaces.JobRow) bool {
		return row.Name == ref
	}, "job", "agent")
	if err != nil {
		writeResolveErr(w, err, "job or agent")
		return
	}
	rs := skills.RunScope{WorkspaceID: row.WorkspaceID}
	if row.Kind == string(skills.ScopeAgent) {
		rs.AgentID = row.ID
	} else {
		rs.JobID = row.ID
	}
	list, err := s.skills.Resolve(ctx, rs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []skills.Resolved{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": list, "effective_for": map[string]string{"id": row.ID, "name": row.Name, "kind": row.Kind}})
}

func (s *Server) handleSkillsCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Name       string         `json:"name"`
		SkillMD    string         `json:"skill_md"`
		Provenance string         `json:"provenance"`
		Source     *skills.Source `json:"source"`
		Scope      skills.Scope   `json:"scope"`
		Owner      string         `json:"owner"`
		Workspace  string         `json:"workspace"`
		Note       string         `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	a, hasActor := tools.ActorFrom(ctx)
	run := hasActor && (a.Kind == "agent" || a.Kind == "job")
	opts := skills.CreateOpts{SkillMD: body.SkillMD, Provenance: skills.Provenance(body.Provenance), Source: body.Source, Note: body.Note}
	ref := skills.Ref{Scope: body.Scope, Name: body.Name}
	switch {
	case ref.Scope == "" && run:
		// D4: a run keeps what it makes in its own scope.
		ref.Scope, ref.Owner = skills.Scope(a.Kind), a.JobID
	case ref.Scope == "":
		ref.Scope = skills.ScopeGlobal
	default:
		id, err := s.skillOwnerID(ctx, ref.Scope, body.Owner, body.Workspace)
		if err != nil {
			s.writeSkillErr(w, err)
			return
		}
		ref.Owner = id
	}
	opts.Ref = ref
	if !s.requireSkillWrite(w, r, ref) {
		return
	}
	if hasActor {
		opts.CreatedBy = &skills.CreatedBy{ActorKind: a.Kind, Job: a.Job, RunID: a.RunID}
	} else {
		opts.CreatedBy = &skills.CreatedBy{ActorKind: "user"}
	}
	if run {
		opts.Provenance = skills.ProvenanceAgent
		if kit := s.ws.OwnerKit(ctx, a.WorkspaceID, a.Job); kit != nil && kit.SkillPolicy != nil {
			opts.Pending = kit.SkillPolicy.Reviewed()
		}
	}
	sk, err := s.skills.Create(ctx, opts)
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

func (s *Server) handleSkillGet(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.skillRef(w, r, false)
	if !ok {
		return
	}
	sk, err := s.skills.Get(r.Context(), ref, true)
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) handleSkillPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SkillMD   string          `json:"skill_md"`
		Harnesses map[string]bool `json:"harnesses"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	ref, ok := s.skillRef(w, r, true)
	if !ok || !s.requireSkillWrite(w, r, ref) {
		return
	}
	sk, err := s.skills.Update(r.Context(), ref, body.SkillMD, r.Header.Get("If-Match"), body.Harnesses)
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) handleSkillDelete(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.skillRef(w, r, true)
	if !ok || !s.requireSkillWrite(w, r, ref) {
		return
	}
	if err := s.skills.Delete(r.Context(), ref); err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name"), "ref": ref.String()})
}

// handleSkillRefresh pulls the skill source again. A skill with no source
// comes back unchanged, with updated false.
func (s *Server) handleSkillRefresh(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.skillRef(w, r, true)
	if !ok || !s.requireSkillWrite(w, r, ref) {
		return
	}
	sk, updated, err := s.skills.Refresh(r.Context(), ref)
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	resp := map[string]any{"skill": sk, "updated": updated}
	if sk.Source == nil {
		resp["note"] = "skill has no source"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSkillMove puts a skill in another scope or under another owner. The
// caller must be able to write both the source and the target.
func (s *Server) handleSkillMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		ToScope     skills.Scope `json:"to_scope"`
		ToOwner     string       `json:"to_owner"`
		ToWorkspace string       `json:"to_workspace"`
		Replace     bool         `json:"replace"`
		Rename      string       `json:"rename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	from, ok := s.skillRef(w, r, true)
	if !ok || !s.requireSkillWrite(w, r, from) {
		return
	}
	if body.ToScope == "" {
		writeErr(w, http.StatusBadRequest, "validation", "to_scope is required")
		return
	}
	toWS := body.ToWorkspace
	if toWS == "" {
		toWS = s.skillWorkspace(ctx, from)
	}
	owner, err := s.skillOwnerID(ctx, body.ToScope, body.ToOwner, toWS)
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	to := skills.Ref{Scope: body.ToScope, Owner: owner, Name: from.Name}
	if body.Rename != "" {
		to.Name = body.Rename
	}
	if !s.requireSkillWrite(w, r, to) {
		return
	}
	sk, err := s.skills.Move(ctx, from, skills.Target{Scope: to.Scope, Owner: to.Owner}, skills.MoveOpts{Replace: body.Replace, Rename: body.Rename})
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// handleSkillApprove makes a pending skill active. Only a person or the
// assistant approves; a run cannot approve its own skill.
func (s *Server) handleSkillApprove(w http.ResponseWriter, r *http.Request) {
	if rejectAnyRunToken(w, r) {
		return
	}
	ref, ok := s.skillRef(w, r, true)
	if !ok || !s.requireSkillWrite(w, r, ref) {
		return
	}
	sk, err := s.skills.Approve(r.Context(), ref)
	if err != nil {
		s.writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) writeSkillErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, skills.ErrInherited), errors.Is(err, skills.ErrManaged),
		errors.Is(err, skills.ErrConflict), errors.Is(err, skills.ErrExists):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, skills.ErrArchived):
		writeErr(w, http.StatusConflict, "archived", "Unarchive the skill first.")
	case errors.Is(err, skills.ErrPrecondition):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.Is(err, skills.ErrNoOwner), errors.Is(err, os.ErrNotExist), errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	}
}
