package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

func (s *Server) routeHarnessVersions(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/harnesses/{name}/install", s.handleHarnessInstall)
	mux.HandleFunc("POST /v1/harnesses/{name}/update", s.handleHarnessUpdate)
	mux.HandleFunc("POST /v1/harnesses/{name}/versions/check", s.handleHarnessVersionsCheck)
}

// handleHarnessInstall installs the harness program, or the version given.
func (s *Server) handleHarnessInstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	if _, err := s.harness.Install(r.Context(), name, body.Version); err != nil {
		writeVisorErr(w, err)
		return
	}
	s.invalidateDetect(name)
	writeJSON(w, http.StatusOK, s.harnessRowFor(r.Context(), name))
}

// handleHarnessUpdate updates the harness now. A session that uses the
// harness moves the update to the end of the session: the reply is 202. A
// failed update is in last_update of the row.
func (s *Server) handleHarnessUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	_, err := s.harness.Update(r.Context(), name)
	switch {
	case errors.Is(err, agentvisor.ErrBusy):
		writeJSON(w, http.StatusAccepted, s.harnessRowFor(r.Context(), name))
		return
	case errors.Is(err, agentvisor.ErrNotFound), errors.Is(err, agentvisor.ErrUnsupported), errors.Is(err, harness.ErrNoVisor):
		writeVisorErr(w, err)
		return
	case err != nil:
		s.log.Warn("harness update", "harness", name, "err", err)
	}
	writeJSON(w, http.StatusOK, s.harnessRowFor(r.Context(), name))
}

// handleHarnessVersionsCheck looks for a new release now.
func (s *Server) handleHarnessVersionsCheck(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	if err := s.harness.CheckVersions(r.Context()); err != nil {
		writeVisorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.harnessRowFor(r.Context(), name))
}

// versionRow adds the version fields to a harness row.
func (s *Server) versionRow(ctx context.Context, cfg *config.Config, name string, row map[string]any) {
	updates := harness.UpdatesAuto
	if cfg != nil {
		if p := cfg.Tools.Pin(name); p != nil && p.Updates == harness.UpdatesKeep {
			updates = harness.UpdatesKeep
		}
	}
	v, _ := s.harness.Version(ctx, name)
	if v.Installed && row["installed"] != true {
		row["installed"], row["bin_path"] = true, v.Path
	}
	row["version"] = v.Version
	row["latest_version"] = v.Latest
	row["update_available"] = v.Latest != "" && v.Version != "" && v.Latest != v.Version
	row["updates"] = updates
	row["can_install"] = v.Installable && !v.Installed
	row["can_update"] = v.Installable || v.Source == agentvisor.SourceVisor
	row["can_choose_version"] = v.Installable
	row["updating"] = v.Updating || v.Pending
	row["installs"] = v.Installs
	row["source"] = emptyNil(string(v.Source))
	row["last_update"] = nil
	if n := len(v.History); n > 0 {
		u := v.History[n-1]
		row["last_update"] = map[string]any{"at": u.At.UTC().Format(time.RFC3339), "from": u.From, "to": u.To, "ok": u.OK, "error": emptyNil(u.Error)}
	}
}
