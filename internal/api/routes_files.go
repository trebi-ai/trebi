package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeFiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{id}/files", s.handleFilesList)
	mux.HandleFunc("GET /v1/workspaces/{id}/files/{path...}", s.handleFilesGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/files/{path...}", s.handleFilesPut)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/files/{path...}", s.handleFilesDelete)
	mux.HandleFunc("POST /v1/workspaces/{id}/files/move", s.inWorkspaceFiles(s.filesMove))
	mux.HandleFunc("POST /v1/workspaces/{id}/folders/{path...}", s.inWorkspaceFiles(s.foldersCreate))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/folders/{path...}", s.inWorkspaceFiles(s.foldersDelete))

	// Job-scoped alias: same bytes plane, jail rooted at the job folder.
	// Request/response paths are job-relative; bus attrs.path is workspace-relative.
	// Agents resolve too (index name <name>), so the agent Files tab
	// browses agents/<name>/ through the same routes.
	mux.HandleFunc("GET /v1/jobs/{name}/files", s.handleJobFilesList)
	mux.HandleFunc("GET /v1/jobs/{name}/files/{path...}", s.handleJobFilesGet)
	mux.HandleFunc("PUT /v1/jobs/{name}/files/{path...}", s.handleJobFilesPut)
	mux.HandleFunc("DELETE /v1/jobs/{name}/files/{path...}", s.handleJobFilesDelete)
	mux.HandleFunc("POST /v1/jobs/{name}/files/move", s.inJobFiles(s.filesMove))
	mux.HandleFunc("POST /v1/jobs/{name}/folders/{path...}", s.inJobFiles(s.foldersCreate))
	mux.HandleFunc("DELETE /v1/jobs/{name}/folders/{path...}", s.inJobFiles(s.foldersDelete))

	mux.HandleFunc("GET /v1/skills/{name}/files", s.handleSkillFilesList)
	mux.HandleFunc("GET /v1/skills/{name}/files/{path...}", s.handleSkillFilesGet)
	mux.HandleFunc("PUT /v1/skills/{name}/files/{path...}", s.handleSkillFilesPut)
	mux.HandleFunc("DELETE /v1/skills/{name}/files/{path...}", s.handleSkillFilesDelete)
	mux.HandleFunc("POST /v1/skills/{name}/files/move", s.inSkillFiles(s.filesMove))
	mux.HandleFunc("POST /v1/skills/{name}/folders/{path...}", s.inSkillFiles(s.foldersCreate))
	mux.HandleFunc("DELETE /v1/skills/{name}/folders/{path...}", s.inSkillFiles(s.foldersDelete))
}

// fileOp is one file-plane handler after its owner scope resolves.
type fileOp func(fileScope, http.ResponseWriter, *http.Request)

func (s *Server) inWorkspaceFiles(op fileOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if scope, ok := s.workspaceFileScope(w, r); ok {
			op(scope, w, r)
		}
	}
}

func (s *Server) inJobFiles(op fileOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if scope, ok := s.jobFileScope(w, r); ok {
			op(scope, w, r)
		}
	}
}

// inSkillFiles serves the mutating skill routes only.
func (s *Server) inSkillFiles(op fileOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if scope, ok := s.skillFileScope(w, r, true); ok {
			op(scope, w, r)
		}
	}
}

// fileScope is one resolved jail for the file handlers.
type fileScope struct {
	ws          *workspaces.Workspace // nil for skill scope
	root        string                // jail root: ws.Path, job folder, or skill dir
	wsPrefix    string                // "" for workspace/skill; "<job-dir>/" for job scope
	denyDocs    bool                  // job scope: refuse PUT/DELETE of indexed markdown
	denySkillMD bool                  // skill scope: refuse PUT/DELETE of SKILL.md
	noun        string                // what the confirm text calls the jail root
	emit        func(ctx context.Context, rel, action string)
}

func (sc fileScope) wsRel(rel string) string {
	if sc.wsPrefix == "" {
		return rel
	}
	return sc.wsPrefix + rel
}

func (s *Server) workspaceFileScope(w http.ResponseWriter, r *http.Request) (fileScope, bool) {
	ws, ok := s.pathWorkspace(w, r)
	if !ok {
		return fileScope{}, false
	}
	return fileScope{ws: ws, root: ws.Path, noun: "the workspace", emit: s.fileEmitter(ws.ID)}, true
}

func (s *Server) fileEmitter(wsID string) func(context.Context, string, string) {
	return func(ctx context.Context, rel, action string) {
		s.emitFileBus(ctx, wsID, rel, action)
	}
}

func (s *Server) jobFileScope(w http.ResponseWriter, r *http.Request) (fileScope, bool) {
	row, ok := s.resolveFileRow(w, r, r.PathValue("name"))
	if !ok {
		return fileScope{}, false
	}
	ws, err := s.ws.Get(r.Context(), row.WorkspaceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return fileScope{}, false
	}
	jobDir := filepath.Dir(row.Path)
	rel, err := workspaceRel(ws.Path, jobDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return fileScope{}, false
	}
	prefix := ""
	if rel != "." && rel != "" {
		prefix = strings.TrimSuffix(rel, "/") + "/"
	}
	return fileScope{ws: ws, root: jobDir, wsPrefix: prefix, denyDocs: true, noun: "the job folder", emit: s.fileEmitter(ws.ID)}, true
}

func (s *Server) skillFileScope(w http.ResponseWriter, r *http.Request, mutating bool) (fileScope, bool) {
	ref, ok := s.skillRef(w, r, mutating)
	if !ok {
		return fileScope{}, false
	}
	sk, err := s.skills.Get(r.Context(), ref, false)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "skill not found")
		return fileScope{}, false
	}
	if mutating {
		if !s.requireSkillWrite(w, r, ref) {
			return fileScope{}, false
		}
		if !skillFilesWritable(sk.Provenance) {
			writeErr(w, http.StatusConflict, "conflict",
				"skill file writes are denied for provenance "+string(sk.Provenance)+"; copy to a local skill")
			return fileScope{}, false
		}
	}
	return fileScope{
		root: sk.Path, denySkillMD: true, noun: "skill " + sk.Name,
		emit: func(ctx context.Context, _, _ string) {
			_ = s.skills.Touch(ctx, ref)
		},
	}, true
}

func skillFilesWritable(p skills.Provenance) bool {
	return p == skills.ProvenanceLocal || p == skills.ProvenanceAssistant || p == skills.ProvenanceAgent || p == ""
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.workspaceFileScope(w, r)
	if !ok {
		return
	}
	s.filesList(scope, w, r)
}

func (s *Server) handleFilesGet(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.workspaceFileScope(w, r)
	if !ok {
		return
	}
	s.filesGet(scope, w, r)
}

func (s *Server) handleFilesPut(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.workspaceFileScope(w, r)
	if !ok {
		return
	}
	s.filesPut(scope, w, r)
}

func (s *Server) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.workspaceFileScope(w, r)
	if !ok {
		return
	}
	s.filesDelete(scope, w, r)
}

func (s *Server) handleJobFilesList(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.jobFileScope(w, r)
	if !ok {
		return
	}
	s.filesList(scope, w, r)
}

func (s *Server) handleJobFilesGet(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.jobFileScope(w, r)
	if !ok {
		return
	}
	s.filesGet(scope, w, r)
}

func (s *Server) handleJobFilesPut(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.jobFileScope(w, r)
	if !ok {
		return
	}
	s.filesPut(scope, w, r)
}

func (s *Server) handleJobFilesDelete(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.jobFileScope(w, r)
	if !ok {
		return
	}
	s.filesDelete(scope, w, r)
}

func (s *Server) handleSkillFilesList(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.skillFileScope(w, r, false)
	if !ok {
		return
	}
	s.filesList(scope, w, r)
}

func (s *Server) handleSkillFilesGet(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.skillFileScope(w, r, false)
	if !ok {
		return
	}
	s.filesGet(scope, w, r)
}

func (s *Server) handleSkillFilesPut(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.skillFileScope(w, r, true)
	if !ok {
		return
	}
	s.filesPut(scope, w, r)
}

func (s *Server) handleSkillFilesDelete(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.skillFileScope(w, r, true)
	if !ok {
		return
	}
	s.filesDelete(scope, w, r)
}

func (s *Server) filesList(scope fileScope, w http.ResponseWriter, r *http.Request) {
	items, next, err := s.files.List(r.Context(), scope.root, files.ListOpts{
		Prefix:    r.URL.Query().Get("prefix"),
		AfterPath: r.URL.Query().Get("cursor"),
		Limit:     parseLimit(r),
		Tree:      r.URL.Query().Get("recursive") == "1",
	})
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	out := map[string]any{"files": items, "truncated": next != ""}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) filesGet(scope fileScope, w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if r.URL.Query().Get("stat") == "1" {
		info, err := s.files.Stat(r.Context(), scope.root, rel)
		if err != nil {
			writeFilesErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"path":         info.Path,
			"size_bytes":   info.SizeBytes,
			"mtime":        info.MTime.UTC().Format(time.RFC3339),
			"hash":         info.Hash,
			"content_type": contentTypeFor(info.Path),
		})
		return
	}
	data, info, err := s.files.Read(r.Context(), scope.root, rel)
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	if r.URL.Query().Get("encoding") == "base64" {
		setETagHash(w, info.Hash)
		writeJSON(w, http.StatusOK, map[string]any{
			"path": info.Path, "encoding": "base64", "hash": info.Hash,
			"size_bytes": info.SizeBytes, "content_type": contentTypeFor(info.Path),
			"content": base64.StdEncoding.EncodeToString(data),
		})
		return
	}
	etag := `"` + info.Hash + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", info.MTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if noneMatchOK(r, info.Hash) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(info.Path))
	_, _ = w.Write(data)
}

func (s *Server) filesPut(scope fileScope, w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if !s.allowFileMutation(w, r, scope, rel) {
		return
	}
	data, ok := s.readFileBody(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("encoding") == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "body is not valid base64: "+err.Error())
			return
		}
		data = decoded
	}
	if scope.ws != nil && s.agentOwnAgentMarkdown(r, scope.ws.ID, scope.wsRel(rel)) {
		s.proposeAgentMarkdown(scope.ws, scope.wsRel(rel), data, w, r)
		return
	}
	info, created, err := s.files.Write(r.Context(), scope.root, rel, data, ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	s.afterFileWrite(r.Context(), scope, info.Path, created)
	setETagHash(w, info.Hash)
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{
		"path": info.Path, "hash": info.Hash, "size_bytes": info.SizeBytes, "created": created,
	})
}

func (s *Server) filesDelete(scope fileScope, w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if scope.ws != nil && s.agentOwnAgentMarkdown(r, scope.ws.ID, scope.wsRel(rel)) {
		writeForbidden(w, "AGENT.md delete requires a human")
		return
	}
	if !s.allowFileMutation(w, r, scope, rel) {
		return
	}
	if err := s.files.Delete(r.Context(), scope.root, rel, ifMatchHeader(r)); err != nil {
		writeFilesErr(w, err)
		return
	}
	busRel := scope.wsRel(rel)
	if scope.ws != nil && indexedMarkdown(busRel) {
		_ = s.ws.RebuildIndex(r.Context(), scope.ws)
	}
	if scope.emit != nil {
		scope.emit(r.Context(), busRel, "deleted")
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rel})
}

func (s *Server) foldersCreate(scope fileScope, w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if !s.allowFileMutation(w, r, scope, rel) {
		return
	}
	info, err := s.files.Mkdir(r.Context(), scope.root, rel)
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	if scope.emit != nil {
		scope.emit(r.Context(), scope.wsRel(info.Path), "created")
	}
	writeJSON(w, http.StatusCreated, info)
}

// foldersDelete asks for confirm itself, because its text needs the count
// of files in the resolved folder.
func (s *Server) foldersDelete(scope fileScope, w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	paths, ok := s.allowFolderMutation(w, r, scope, rel, "")
	if !ok {
		return
	}
	if !confirmed(r) {
		writeConfirmRequired(w, "deletes "+strings.TrimSuffix(rel, "/")+"/ and the "+
			strconv.Itoa(len(paths))+" files in it from "+scope.noun)
		return
	}
	n, err := s.files.DeleteDir(r.Context(), scope.root, rel)
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	s.afterFolderChange(r.Context(), scope, paths)
	if scope.emit != nil {
		scope.emit(r.Context(), scope.wsRel(strings.Trim(rel, "/")), "deleted")
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rel, "files_removed": n})
}

// fileMoveRequest is the POST <owner>/files/move body.
type fileMoveRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) filesMove(scope fileScope, w http.ResponseWriter, r *http.Request) {
	var body fileMoveRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.From == "" || body.To == "" {
		writeErr(w, http.StatusBadRequest, "validation", "from and to are required")
		return
	}
	for _, rel := range []string{body.From, body.To} {
		if scope.ws != nil && s.agentOwnAgentMarkdown(r, scope.ws.ID, scope.wsRel(rel)) {
			writeForbidden(w, "AGENT.md move requires a human")
			return
		}
		if !s.allowFileMutation(w, r, scope, rel) {
			return
		}
	}
	paths, ok := s.allowFolderMutation(w, r, scope, body.From, body.To)
	if !ok {
		return
	}
	info, err := s.files.Move(r.Context(), scope.root, body.From, body.To, files.MoveOpts{
		IfMatch: ifMatchHeader(r), Overwrite: body.Overwrite,
	})
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	// A moved file keeps its base name, so the old paths name the documents.
	s.afterFolderChange(r.Context(), scope, append(paths, body.From, info.Path))
	if scope.emit != nil {
		scope.emit(r.Context(), scope.wsRel(strings.Trim(body.From, "/")), "deleted")
		scope.emit(r.Context(), scope.wsRel(info.Path), "created")
	}
	setETagHash(w, info.Hash)
	writeJSON(w, http.StatusOK, info)
}

// allowFolderMutation applies the file rules to each file in a folder, and
// to its new path when to is set. A file source returns no paths.
func (s *Server) allowFolderMutation(w http.ResponseWriter, r *http.Request, scope fileScope, rel, to string) ([]string, bool) {
	paths, err := s.files.DirFiles(r.Context(), scope.root, rel)
	if errors.Is(err, files.ErrNotDir) && to != "" {
		return nil, true
	}
	if err != nil {
		writeFilesErr(w, err)
		return nil, false
	}
	from := strings.Trim(rel, "/")
	for _, p := range paths {
		targets := []string{p}
		if to != "" {
			targets = append(targets, strings.Trim(to, "/")+strings.TrimPrefix(p, from))
		}
		for _, t := range targets {
			if scope.ws != nil && s.agentOwnAgentMarkdown(r, scope.ws.ID, scope.wsRel(t)) {
				writeForbidden(w, "a folder with AGENT.md needs a human")
				return nil, false
			}
			if !s.allowFileMutation(w, r, scope, t) {
				return nil, false
			}
		}
	}
	return paths, true
}

// afterFolderChange rebuilds the index when a changed path is a document.
func (s *Server) afterFolderChange(ctx context.Context, scope fileScope, paths []string) {
	if scope.ws == nil {
		return
	}
	for _, p := range paths {
		if indexedMarkdown(scope.wsRel(p)) {
			_ = s.ws.RebuildIndex(ctx, scope.ws)
			return
		}
	}
}

func (s *Server) allowFileMutation(w http.ResponseWriter, r *http.Request, scope fileScope, rel string) bool {
	if scope.ws != nil {
		if agent, ok := scopeFrom(r.Context()); ok && !agent.canWritePath(scope.ws.ID, scope.wsRel(rel)) {
			writeForbidden(w, "agent may write only under "+agent.dir+"/")
			return false
		}
	}
	if scope.denySkillMD && strings.EqualFold(filepath.Base(rel), "skill.md") {
		writeErr(w, http.StatusConflict, "conflict", "use PUT /v1/skills/{name} (trebi skill update) for SKILL.md")
		return false
	}
	// An agent's proposal path is resolved by the files handler. Bypass the
	// job-scoped typed-document guard only for its own AGENT.md.
	if scope.ws != nil && s.agentOwnAgentMarkdown(r, scope.ws.ID, scope.wsRel(rel)) {
		return true
	}
	if !scope.denyDocs {
		return true
	}
	if msg := typedDocMessage(rel); msg != "" {
		writeErr(w, http.StatusConflict, "conflict", msg)
		return false
	}
	return true
}

func typedDocMessage(rel string) string {
	switch strings.ToLower(filepath.Base(rel)) {
	case "job.md":
		return "use PUT /v1/jobs/{name} (trebi job) for JOB.md"
	case "memory.md":
		return "use the memory routes (trebi job memory) for MEMORY.md"
	case "agent.md":
		return "use the agent routes (trebi agent) for AGENT.md"
	}
	return ""
}

func (s *Server) requireIfMatch(w http.ResponseWriter, r *http.Request, root, rel string) bool {
	if err := s.files.CheckIfMatch(r.Context(), root, rel, ifMatchHeader(r)); err != nil {
		writeFilesErr(w, err)
		return false
	}
	return true
}

func workspaceRel(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// findWorkspace resolves a workspace id or name. It writes 404 for an unknown
// ref and 500 for a store error.
func (s *Server) findWorkspace(ctx context.Context, w http.ResponseWriter, ref string) (*workspaces.Workspace, bool) {
	ws, err := s.ws.Resolve(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, false
	}
	return ws, true
}

// pathWorkspace resolves the {id} path value (id or name) to a workspace.
func (s *Server) pathWorkspace(w http.ResponseWriter, r *http.Request) (*workspaces.Workspace, bool) {
	return s.findWorkspace(r.Context(), w, r.PathValue("id"))
}

func (s *Server) readFileBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	max := s.files.MaxBytes()
	if r.ContentLength > max {
		writeErr(w, http.StatusRequestEntityTooLarge, "validation", "file exceeds size limit")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "validation", "file exceeds size limit")
			return nil, false
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return nil, false
	}
	return data, true
}

func (s *Server) afterFileWrite(ctx context.Context, scope fileScope, rel string, created bool) {
	busRel := scope.wsRel(rel)
	if scope.ws != nil && indexedMarkdown(busRel) {
		_ = s.ws.RebuildIndex(ctx, scope.ws)
	}
	action := "updated"
	if created {
		action = "created"
	}
	if scope.emit != nil {
		scope.emit(ctx, busRel, action)
	}
}

func (s *Server) emitFileBus(ctx context.Context, wsID, rel, action string) {
	s.appendBus(ctx, store.BusEvent{
		Topic: "file." + action, Entity: "file", EntityID: wsID + ":" + rel, Action: action,
		Attrs: map[string]string{"workspace": wsID, "path": rel},
	})
}

func writeFilesErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrEscapesRoot), errors.Is(err, files.ErrIsDir),
		errors.Is(err, files.ErrNotDir), errors.Is(err, files.ErrIntoItself):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, files.ErrProtectedPath), errors.Is(err, files.ErrExists):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, files.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "validation", "file exceeds size limit")
	case errors.Is(err, files.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "file not found")
	case errors.Is(err, files.ErrStale):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", staleMessage(err))
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func staleMessage(err error) string {
	var se *files.StaleError
	if errors.As(err, &se) && !se.Missing && se.Hash != "" {
		return "If-Match mismatch; current hash: " + se.Hash
	}
	return "If-Match set but file is missing"
}

func ifMatchHeader(r *http.Request) string {
	return strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
}

func setETagHash(w http.ResponseWriter, hash string) {
	if hash != "" {
		w.Header().Set("ETag", `"`+hash+`"`)
	}
}

func noneMatchOK(r *http.Request, hash string) bool {
	inm := strings.Trim(strings.TrimSpace(r.Header.Get("If-None-Match")), `"`)
	return inm != "" && inm == hash
}

func indexedMarkdown(rel string) bool {
	switch strings.ToLower(filepath.Base(rel)) {
	case "job.md", "trebi.md", "agent.md", "memory.md":
		return true
	}
	return false
}

func contentTypeFor(rel string) string {
	ext := strings.ToLower(filepath.Ext(rel))
	switch ext {
	case ".py":
		return "text/x-python"
	case ".md":
		return "text/markdown"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
