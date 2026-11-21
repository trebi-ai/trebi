package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/uploads"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeUploads(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/workspaces/{id}/uploads", s.handleUploadPut)
	mux.HandleFunc("GET /v1/workspaces/{id}/uploads/{uid}", s.handleUploadGet)
}

// uploadWorkspace resolves the workspace of an upload route. An agent caller
// reaches only its own workspace.
func (s *Server) uploadWorkspace(w http.ResponseWriter, r *http.Request) (*workspaces.Workspace, bool) {
	ws, ok := s.pathWorkspace(w, r)
	if !ok {
		return nil, false
	}
	if a, ok := tools.ActorFrom(r.Context()); ok && a.WorkspaceID != "" && a.WorkspaceID != ws.ID {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return nil, false
	}
	return ws, true
}

// handleUploadPut stores the raw body as an image. With ?path= it stores a
// workspace file instead.
func (s *Server) handleUploadPut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.uploadWorkspace(w, r)
	if !ok {
		return
	}
	var (
		u   uploads.Upload
		err error
	)
	if p := r.URL.Query().Get("path"); p != "" {
		u, err = s.uploadFromPath(r.Context(), ws, p)
	} else {
		if r.ContentLength > s.uploads.MaxBytes() {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", uploads.ErrTooLarge.Error())
			return
		}
		u, err = s.uploads.Put(r.Context(), ws.ID, r.Body)
	}
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleUploadGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.uploadWorkspace(w, r)
	if !ok {
		return
	}
	f, u, err := s.uploads.Open(r.Context(), ws.ID, r.PathValue("uid"))
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", u.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(u.Size, 10))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, f)
}

// uploadFromPath stores one workspace file as an upload. An absolute path
// must be in the workspace folder.
func (s *Server) uploadFromPath(ctx context.Context, ws *workspaces.Workspace, p string) (uploads.Upload, error) {
	rel := p
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(ws.Path, p)
		if err != nil {
			return uploads.Upload{}, fmt.Errorf("%w: %s", files.ErrEscapesRoot, p)
		}
		rel = filepath.ToSlash(r)
	}
	data, _, err := s.files.Read(ctx, ws.Path, rel)
	if err != nil {
		return uploads.Upload{}, err
	}
	return s.uploads.Put(ctx, ws.ID, bytes.NewReader(data))
}

func writeUploadErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, uploads.ErrTooLarge), errors.Is(err, files.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, uploads.ErrType):
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
	case errors.Is(err, uploads.ErrNotFound), errors.Is(err, files.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, files.ErrEscapesRoot), errors.Is(err, files.ErrProtectedPath), errors.Is(err, files.ErrIsDir):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// attachUploads resolves the attachments of one message in workspace wsID.
// An item is an upload id or a workspace file path. An unknown id, an id of
// another workspace, or a bad file is a validation error.
func (s *Server) attachUploads(ctx context.Context, w http.ResponseWriter, wsID string, items []string) (uploads.Attachments, bool) {
	if len(items) == 0 {
		return nil, true
	}
	ids := make([]string, 0, len(items))
	var ws *workspaces.Workspace
	for _, it := range items {
		it = strings.TrimSpace(it)
		if strings.HasPrefix(it, uploads.IDPrefix) {
			ids = append(ids, it)
			continue
		}
		if ws == nil {
			var err error
			if ws, err = s.ws.Get(ctx, wsID); err != nil {
				writeErr(w, http.StatusBadRequest, "validation", "attachments: workspace not found")
				return nil, false
			}
		}
		u, err := s.uploadFromPath(ctx, ws, it)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "attachments: "+err.Error())
			return nil, false
		}
		ids = append(ids, u.ID)
	}
	atts, err := s.uploads.Attach(ctx, wsID, ids)
	if errors.Is(err, uploads.ErrNotFound) {
		writeErr(w, http.StatusBadRequest, "validation", "attachments: "+err.Error())
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, false
	}
	return atts, true
}
