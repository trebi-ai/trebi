package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

func (s *Server) routeHarnessVersions(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/harnesses/{name}/update", s.handleHarnessUpdate)
	mux.HandleFunc("POST /v1/harnesses/{name}/versions/check", s.handleHarnessVersionsCheck)
}

// handleHarnessUpdate updates the harness now. A run that uses the harness
// moves the update to the end of the run: the reply is 202. A failed check
// is in last_update of the row.
func (s *Server) handleHarnessUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	err := s.versions.RequestUpdate(r.Context(), name)
	switch {
	case errors.Is(err, harness.ErrHarnessBusy):
		writeJSON(w, http.StatusAccepted, s.harnessRowFor(r.Context(), name))
		return
	case errors.Is(err, harness.ErrNoVersionSource), errors.Is(err, harness.ErrNotInstalled):
		writeVersionErr(w, err)
		return
	case err != nil:
		s.log.Warn("harness update", "harness", name, "err", err)
	}
	writeJSON(w, http.StatusOK, s.harnessRowFor(r.Context(), name))
}

// handleHarnessVersionsCheck reads the installed and the latest version now.
func (s *Server) handleHarnessVersionsCheck(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	if _, err := s.versions.Check(r.Context(), name); err != nil {
		writeVersionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.harnessRowFor(r.Context(), name))
}

// applyVersionPatch applies updates and version after the config write:
// keep copies the program, auto removes the kept copy, and a version
// installs and keeps that version.
func (s *Server) applyVersionPatch(ctx context.Context, name string, p harness.Patch) error {
	version := ""
	if p.Version != nil {
		version = *p.Version
	}
	switch {
	case p.Updates != nil && *p.Updates == harness.UpdatesAuto:
		return s.versions.Auto(ctx, name)
	case p.Updates != nil && *p.Updates == harness.UpdatesKeep:
		return s.versions.Keep(ctx, name, version)
	case version != "":
		keep := harness.UpdatesKeep
		if err := s.harness.Apply(ctx, name, harness.Patch{Updates: &keep}); err != nil {
			return err
		}
		return s.versions.Keep(ctx, name, version)
	}
	return nil
}

func writeVersionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, harness.ErrNoVersionSource):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "Trebi cannot update this harness.")
	case errors.Is(err, harness.ErrNotInstalled):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "The harness program is not installed.")
	case errors.Is(err, harness.ErrNoVersionInstall):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "This harness cannot install a specific version.")
	case errors.Is(err, harness.ErrHarnessBusy):
		writeErr(w, http.StatusConflict, "conflict", "A run uses this harness. Try again when the run ends.")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// versionRow adds the version fields to a harness row.
func (s *Server) versionRow(ctx context.Context, cfg *config.Config, name string, row map[string]any) {
	updates := harness.UpdatesAuto
	if cfg != nil {
		if p := cfg.Tools.Pin(name); p != nil && p.Updates == harness.UpdatesKeep {
			updates = harness.UpdatesKeep
		}
	}
	v, _ := harness.ReadVersionState(ctx, s.st.DB(), name)
	row["version"] = v.Version
	row["latest_version"] = v.Latest
	row["update_available"] = v.UpdateAvailable()
	row["updates"] = updates
	row["checked_at"] = emptyNil(v.CheckedAt)
	row["can_update"] = s.versions.CanUpdate(name)
	row["can_choose_version"] = s.versions.CanInstallVersion(name)
	row["updating"] = s.versions.Updating(name)
	if v.LastUpdate != nil {
		row["last_update"] = v.LastUpdate
	} else {
		row["last_update"] = nil
	}
}
