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
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	items, next, err := s.files.List(r.Context(), ws.Path, files.ListOpts{
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

func (s *Server) handleFilesGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	rel := r.PathValue("path")
	if r.URL.Query().Get("stat") == "1" {
		info, err := s.files.Stat(r.Context(), ws.Path, rel)
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
	data, info, err := s.files.Read(r.Context(), ws.Path, rel)
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

func (s *Server) handleFilesPut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	rel := r.PathValue("path")
	data, ok := s.readFileBody(w, r)
	if !ok {
		return
	}
	info, created, err := s.files.Write(r.Context(), ws.Path, rel, data, ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	s.afterFileWrite(r.Context(), ws, info.Path, created)
	setETagHash(w, info.Hash)
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{
		"path": info.Path, "hash": info.Hash, "size_bytes": info.SizeBytes, "created": created,
	})
}

func (s *Server) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	rel := r.PathValue("path")
	if err := s.files.Delete(r.Context(), ws.Path, rel, ifMatchHeader(r)); err != nil {
		writeFilesErr(w, err)
		return
	}
	if indexedMarkdown(rel) {
		_ = s.ws.RebuildIndex(r.Context(), ws)
	}
	s.emitFileBus(r.Context(), ws.ID, rel, "deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rel})
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
