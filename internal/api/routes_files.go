package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeFiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{id}/files", s.handleFilesList)
	mux.HandleFunc("GET /v1/workspaces/{id}/files/{path...}", s.handleFilesGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/files/{path...}", s.handleFilesPut)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/files/{path...}", s.handleFilesDelete)

	// Job-scoped alias: same bytes plane, jail rooted at the job folder.
	// Request/response paths are job-relative; bus attrs.path is workspace-relative.
	mux.HandleFunc("GET /v1/jobs/{name}/files", s.handleJobFilesList)
	mux.HandleFunc("GET /v1/jobs/{name}/files/{path...}", s.handleJobFilesGet)
	mux.HandleFunc("PUT /v1/jobs/{name}/files/{path...}", s.handleJobFilesPut)
	mux.HandleFunc("DELETE /v1/jobs/{name}/files/{path...}", s.handleJobFilesDelete)
}

// fileScope is one resolved jail for the file handlers.
type fileScope struct {
	ws       *workspaces.Workspace
	root     string // jail root: ws.Path or the job folder
	wsPrefix string // "" for workspace scope; "<job-dir>/" for job scope
	denyDocs bool   // job scope: refuse PUT/DELETE of indexed markdown
}

func (sc fileScope) wsRel(rel string) string {
	if sc.wsPrefix == "" {
		return rel
	}
	return sc.wsPrefix + rel
}

func (s *Server) workspaceFileScope(w http.ResponseWriter, r *http.Request) (fileScope, bool) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return fileScope{}, false
	}
	return fileScope{ws: ws, root: ws.Path}, true
}

func (s *Server) jobFileScope(w http.ResponseWriter, r *http.Request) (fileScope, bool) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
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
	return fileScope{ws: ws, root: jobDir, wsPrefix: prefix, denyDocs: true}, true
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

func (s *Server) filesList(scope fileScope, w http.ResponseWriter, r *http.Request) {
	items, next, err := s.files.List(r.Context(), scope.root, files.ListOpts{
		Prefix:    r.URL.Query().Get("prefix"),
		AfterPath: r.URL.Query().Get("cursor"),
		Limit:     parseLimit(r),
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
	if !s.allowFileMutation(w, scope, rel) {
		return
	}
	data, ok := s.readFileBody(w, r)
	if !ok {
		return
	}
	info, created, err := s.files.Write(r.Context(), scope.root, rel, data, ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	s.afterFileWrite(r.Context(), scope.ws, scope.wsRel(info.Path), created)
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
	if !s.allowFileMutation(w, scope, rel) {
		return
	}
	if err := s.files.Delete(r.Context(), scope.root, rel, ifMatchHeader(r)); err != nil {
		writeFilesErr(w, err)
		return
	}
	wsRel := scope.wsRel(rel)
	if indexedMarkdown(wsRel) {
		_ = s.ws.RebuildIndex(r.Context(), scope.ws)
	}
	s.emitFileBus(r.Context(), scope.ws.ID, wsRel, "deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rel})
}

func (s *Server) allowFileMutation(w http.ResponseWriter, scope fileScope, rel string) bool {
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
	case "trebi.md":
		return "use the assistant routes (trebi assistant) for TREBI.md"
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

func (s *Server) workspaceByID(w http.ResponseWriter, r *http.Request) (*workspaces.Workspace, bool) {
	ws, err := s.ws.Get(r.Context(), r.PathValue("id"))
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

func (s *Server) afterFileWrite(ctx context.Context, ws *workspaces.Workspace, rel string, created bool) {
	if indexedMarkdown(rel) {
		_ = s.ws.RebuildIndex(ctx, ws)
	}
	action := "updated"
	if created {
		action = "created"
	}
	s.emitFileBus(ctx, ws.ID, rel, action)
}

func (s *Server) emitFileBus(ctx context.Context, wsID, rel, action string) {
	s.appendBus(ctx, store.BusEvent{
		Topic: "file." + action, Entity: "file", EntityID: wsID + ":" + rel, Action: action,
		Attrs: map[string]string{"workspace": wsID, "path": rel},
	})
}

func writeFilesErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrEscapesRoot), errors.Is(err, files.ErrIsDir):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, files.ErrProtectedPath):
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

func (s *Server) fileHash(ctx context.Context, root, rel string) string {
	info, err := s.files.Stat(ctx, root, rel)
	if err != nil {
		return ""
	}
	return info.Hash
}

func noneMatchOK(r *http.Request, hash string) bool {
	inm := strings.Trim(strings.TrimSpace(r.Header.Get("If-None-Match")), `"`)
	return inm != "" && inm == hash
}

func indexedMarkdown(rel string) bool {
	switch strings.ToLower(filepath.Base(rel)) {
	case "job.md", "trebi.md", "memory.md":
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
