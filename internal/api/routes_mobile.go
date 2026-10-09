// Paired phones (plan 2026-10-09 B6): pairing offers and the phone list.
// The relay hub serves the phones; these routes only manage them.
package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/mobile"
)

func (s *Server) routeMobile(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/mobile/pairings", s.handleMobilePair)
	mux.HandleFunc("DELETE /v1/mobile/pairings/{id}", s.handleMobilePairCancel)
	mux.HandleFunc("GET /v1/mobile/clients", s.handleMobileList)
	mux.HandleFunc("PATCH /v1/mobile/clients/{id}", s.handleMobileUpdate)
	mux.HandleFunc("DELETE /v1/mobile/clients/{id}", s.handleMobileRemove)
}

// handleMobilePair opens a pairing offer. The link carries the psk and the
// claim, so it is returned once and never listed.
func (s *Server) handleMobilePair(w http.ResponseWriter, r *http.Request) {
	if s.mobileOff(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
		TTL  string `json:"ttl"`
		Uses int    `json:"uses"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	in := mobile.PairInput{Name: body.Name, Uses: body.Uses}
	if body.TTL != "" {
		d, err := time.ParseDuration(body.TTL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "ttl must be a duration such as 10m or 24h")
			return
		}
		in.TTL = d
	}
	o, err := s.mobile.Pair(r.Context(), in)
	if err != nil {
		writeMobileErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (s *Server) handleMobilePairCancel(w http.ResponseWriter, r *http.Request) {
	if s.mobileOff(w) {
		return
	}
	if err := s.mobile.CancelOffer(r.PathValue("id")); err != nil {
		writeMobileErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMobileList(w http.ResponseWriter, r *http.Request) {
	if s.mobileOff(w) {
		return
	}
	items, err := s.mobile.List(r.Context())
	if err != nil {
		writeMobileErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleMobileUpdate renames a phone or changes its scope. A scope change
// ends the live sessions of that phone.
func (s *Server) handleMobileUpdate(w http.ResponseWriter, r *http.Request) {
	if s.mobileOff(w) {
		return
	}
	var body struct {
		Name  *string `json:"name"`
		Scope *string `json:"scope"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	c, err := s.mobile.Update(r.Context(), r.PathValue("id"), mobile.UpdateInput{Name: body.Name, Scope: body.Scope})
	if err != nil {
		writeMobileErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleMobileRemove removes a phone here and in the cloud. The confirm gate wraps it.
func (s *Server) handleMobileRemove(w http.ResponseWriter, r *http.Request) {
	if s.mobileOff(w) {
		return
	}
	if err := s.mobile.Remove(r.Context(), r.PathValue("id")); err != nil {
		writeMobileErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mobileOff answers when the daemon has no phone service (a test server).
func (s *Server) mobileOff(w http.ResponseWriter) bool {
	if s.mobile != nil {
		return false
	}
	writeErr(w, http.StatusServiceUnavailable, "internal", "phone access is not available on this daemon")
	return true
}

func writeMobileErr(w http.ResponseWriter, err error) {
	var apiErr *cloudlink.APIError
	switch {
	case errors.Is(err, mobile.ErrNotLinked), errors.Is(err, cloudlink.ErrNotLinked):
		writeErr(w, http.StatusConflict, "not_linked", "Link this machine to Trebi Cloud first.")
	case errors.Is(err, mobile.ErrRemoteAccess):
		writeErr(w, http.StatusConflict, "remote_access_required", "Phone access needs Trebi Pro Cloud.")
	case errors.Is(err, mobile.ErrMobileOff):
		writeErr(w, http.StatusConflict, "mobile_off", "Phone access is off. Turn on Phones and tablets in Settings.")
	case errors.Is(err, mobile.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "phone or pairing not found")
	case errors.Is(err, mobile.ErrValidation):
		writeErr(w, http.StatusBadRequest, "validation", strings.TrimPrefix(err.Error(), mobile.ErrValidation.Error()+": "))
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusForbidden:
			writeErr(w, http.StatusConflict, "remote_access_required", "Phone access needs Trebi Pro Cloud.")
		case http.StatusUnauthorized:
			writeErr(w, http.StatusUnauthorized, "unauthorized", "the cloud rejected this device")
		default:
			writeErr(w, http.StatusBadGateway, "internal", apiErr.Error())
		}
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
