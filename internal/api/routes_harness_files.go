package api

import (
	"encoding/json"
	"errors"
	"net/http"

	agentvisor "github.com/trebi-ai/agent-visor"

	"github.com/trebi-ai/trebi/internal/harness"
)

// maxConfigFileBytes bounds the body of a config file write.
const maxConfigFileBytes = 1 << 20

// Vendor config files of a harness. The routes take a file key from the
// harness table, never a path. The visor writes the files.

func (s *Server) handleHarnessFiles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.harness.Files(r.Context(), r.PathValue("name"))
	if err != nil {
		writeVisorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": rows})
}

func (s *Server) handleHarnessFileRead(w http.ResponseWriter, r *http.Request) {
	fc, err := s.harness.ReadFile(r.Context(), r.PathValue("name"), r.PathValue("key"), r.URL.Query().Get("side"))
	if err != nil {
		writeVisorErr(w, err)
		return
	}
	setETagHash(w, fc.ETag)
	writeJSON(w, http.StatusOK, map[string]string{"content": fc.Content, "etag": fc.ETag})
}

func (s *Server) handleHarnessFileWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxConfigFileBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	fc, err := s.harness.WriteFile(r.Context(), r.PathValue("name"), r.PathValue("key"), body.Content, ifMatchHeader(r))
	if errors.Is(err, agentvisor.ErrStale) {
		setETagHash(w, fc.ETag)
		writeErrDetails(w, http.StatusPreconditionFailed, "precondition_failed", err.Error(),
			map[string]string{"etag": fc.ETag, "content": fc.Content})
		return
	}
	if err != nil {
		writeVisorErr(w, err)
		return
	}
	setETagHash(w, fc.ETag)
	writeJSON(w, http.StatusOK, map[string]string{"etag": fc.ETag})
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
		writeVisorErr(w, err)
		return
	}
	s.handleHarnessFiles(w, r)
}

func (s *Server) handleHarnessFileReset(w http.ResponseWriter, r *http.Request) {
	if err := s.harness.ResetFile(r.Context(), r.PathValue("name"), r.PathValue("key")); err != nil {
		writeVisorErr(w, err)
		return
	}
	s.handleHarnessFiles(w, r)
}

// writeVisorErr maps an error of the harness part of the visor.
func writeVisorErr(w http.ResponseWriter, err error) {
	var parse *harness.ParseError
	switch {
	case errors.As(err, &parse):
		writeErrDetails(w, http.StatusUnprocessableEntity, "validation", parse.Error(), parse)
	case errors.Is(err, harness.ErrUnknownFile), errors.Is(err, agentvisor.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, agentvisor.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, "validation", err.Error())
	case errors.Is(err, agentvisor.ErrUnsupported):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", err.Error())
	case errors.Is(err, agentvisor.ErrState), errors.Is(err, agentvisor.ErrBusy):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, agentvisor.ErrStale):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.Is(err, harness.ErrNoVisor), errors.Is(err, agentvisor.ErrClosed):
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	}
}
