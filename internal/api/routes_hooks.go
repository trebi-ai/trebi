// Hosted webhooks (plan 2026-09-22 Part E): the local registry surface.
// A hook is a hosted object with a local row. The public receiver stays in
// the cloud; this daemon opens no port and only mints, lists, and revokes.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/webhooks"
)

func (s *Server) routeHooks(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/hooks", s.handleHookCreate)
	mux.HandleFunc("GET /v1/hooks", s.handleHooksList)
	mux.HandleFunc("DELETE /v1/hooks/{id}", s.handleHookDelete)
}

// handleHookCreate mints the hosted hook and returns the URL once, in
// full: the token is the URL, so it is a weak credential and the masked
// list is the only other surface that knows it.
func (s *Server) handleHookCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "body is not JSON")
		return
	}
	body.WorkspaceID = strings.TrimSpace(body.WorkspaceID)
	body.Label = strings.TrimSpace(body.Label)
	if body.WorkspaceID == "" || body.Label == "" {
		writeErr(w, http.StatusBadRequest, "validation", "workspace_id and label are required")
		return
	}
	h, err := s.hooks.Create(r.Context(), body.WorkspaceID, body.Label)
	if err != nil {
		writeCloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, hookPayload(h, false))
}

// handleHooksList returns the stored rows, URLs masked unless reveal=1.
// A reveal is an audit entry like the workspace email reveal (Part G).
func (s *Server) handleHooksList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.hooks.List(r.Context())
	if err != nil {
		writeCloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, maskedBody(s, r, "hooks", "hook.revealed", rows, hookPayload))
}

// handleHookDelete revokes the hook in the cloud, then drops the row. The
// confirm gate wraps it.
func (s *Server) handleHookDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.hooks.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeCloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// hookPayload is the wire shape for one hook row (trebi-ui hookSchema).
func hookPayload(h webhooks.Hook, masked bool) map[string]any {
	url := h.URL
	if masked {
		url = maskHookURL(url)
	}
	return map[string]any{
		"id":           h.ID,
		"workspace_id": h.WorkspaceID,
		"label":        h.Label,
		"url":          url,
		"created_at":   h.CreatedAt,
		"masked":       masked,
	}
}

// maskHookURL hides the token and keeps the host recognisable.
func maskHookURL(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[:i+1] + "…"
	}
	return "…"
}

// writeCloudErr maps the hosted-object failures to stable codes: no link,
// a cloud refusal, and a missing row are the three the caller can act on.
func writeCloudErr(w http.ResponseWriter, err error) {
	var apiErr *cloudlink.APIError
	switch {
	case errors.Is(err, cloudlink.ErrNotLinked):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "hook not found")
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusUnauthorized:
			writeErr(w, http.StatusUnauthorized, "unauthorized", "the cloud rejected this device")
		case http.StatusForbidden:
			writeErr(w, http.StatusPreconditionFailed, "precondition_failed", "the plan does not grant hosted_ingress")
		case http.StatusNotFound:
			writeErr(w, http.StatusNotFound, "not_found", "hook not found")
		default:
			writeErr(w, http.StatusBadGateway, "internal", apiErr.Error())
		}
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
