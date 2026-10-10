package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/harness"
)

// Vendor config files of a harness (plan D10). The routes take a file key
// from the harness table, never a path, and never write under the user home.

func (s *Server) handleHarnessFiles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.harness.Files(r.PathValue("name"))
	if err != nil {
		writeHarnessFileErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": rows})
}

func (s *Server) handleHarnessFileRead(w http.ResponseWriter, r *http.Request) {
	content, etag, err := s.harness.ReadFile(r.PathValue("name"), r.PathValue("key"), r.URL.Query().Get("side"))
	if err != nil {
		writeHarnessFileErr(w, err)
		return
	}
	setETagHash(w, etag)
	writeJSON(w, http.StatusOK, map[string]string{"content": content, "etag": etag})
}

func (s *Server) handleHarnessFileWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*harness.MaxConfigFileBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	etag, err := s.harness.WriteFile(r.Context(), r.PathValue("name"), r.PathValue("key"), body.Content, ifMatchHeader(r))
	if err != nil {
		writeHarnessFileErr(w, err)
		return
	}
	setETagHash(w, etag)
	writeJSON(w, http.StatusOK, map[string]string{"etag": etag})
}

func (s *Server) handleHarnessFileOwn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Start string `json:"start"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	name := r.PathValue("name")
	if err := s.harness.OwnFile(r.Context(), name, r.PathValue("key"), body.Start); err != nil {
		writeHarnessFileErr(w, err)
		return
	}
	s.handleHarnessFiles(w, r)
}

func (s *Server) handleHarnessFileReset(w http.ResponseWriter, r *http.Request) {
	if err := s.harness.ResetFile(r.Context(), r.PathValue("name"), r.PathValue("key")); err != nil {
		writeHarnessFileErr(w, err)
		return
	}
	s.handleHarnessFiles(w, r)
}

func writeHarnessFileErr(w http.ResponseWriter, err error) {
	var stale *harness.StaleError
	var parse *harness.ParseError
	switch {
	case errors.As(err, &stale):
		setETagHash(w, stale.ETag)
		writeErrDetails(w, http.StatusPreconditionFailed, "precondition_failed", stale.Error(), stale)
	case errors.As(err, &parse):
		writeErrDetails(w, http.StatusUnprocessableEntity, "validation", parse.Error(), parse)
	case errors.Is(err, harness.ErrUnknownFile):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, harness.ErrNotOwn):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, harness.ErrNoBackup):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	}
}
