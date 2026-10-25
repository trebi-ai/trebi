package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/tmux"
)

func (s *Server) routeHarness(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/harnesses", s.handleHarnesses)
	mux.HandleFunc("GET /v1/harnesses/usage", s.handleHarnessUsage)
	mux.HandleFunc("POST /v1/harnesses/usage/refresh", s.handleHarnessUsageRefresh)
	mux.HandleFunc("PATCH /v1/harnesses/{name}", s.handleHarnessPatch)
	mux.HandleFunc("POST /v1/harnesses/{name}/ensure", s.handleHarnessEnsure)
	mux.HandleFunc("GET /v1/harnesses/{name}/auth", s.handleHarnessAuthGet)
	mux.HandleFunc("GET /v1/harnesses/{name}/models", s.handleHarnessModels)
	mux.HandleFunc("GET /v1/harnesses/{name}/providers", s.handleHarnessProviders)
	mux.HandleFunc("POST /v1/harnesses/{name}/login", s.handleHarnessLoginPost)
	mux.HandleFunc("GET /v1/harnesses/{name}/login", s.handleHarnessLoginGet)
	mux.HandleFunc("DELETE /v1/harnesses/{name}/login", s.handleHarnessLoginDelete)
	mux.HandleFunc("GET /v1/harnesses/{name}/login/attach", s.handleHarnessLoginAttach)
	mux.HandleFunc("POST /v1/harnesses/{name}/logout", s.handleHarnessLogout)
	s.routeHarnessVersions(mux)
	mux.HandleFunc("GET /v1/harnesses/{name}/files", s.handleHarnessFiles)
	mux.HandleFunc("GET /v1/harnesses/{name}/files/{key}", s.handleHarnessFileRead)
	mux.HandleFunc("PUT /v1/harnesses/{name}/files/{key}", s.handleHarnessFileWrite)
	mux.HandleFunc("POST /v1/harnesses/{name}/files/{key}/own", s.handleHarnessFileOwn)
	mux.HandleFunc("DELETE /v1/harnesses/{name}/files/{key}/own", s.handleHarnessFileReset)
}

func (s *Server) handleHarnessUsage(w http.ResponseWriter, r *http.Request) {
	s.writeHarnessUsage(w, r, "")
}

// handleHarnessUsageRefresh reads the Codex limits now. A failed read still
// returns the stored values, with refresh_error.
func (s *Server) handleHarnessUsageRefresh(w http.ResponseWriter, r *http.Request) {
	refreshErr := ""
	if err := s.usage.Refresh(r.Context()); errors.Is(err, harness.ErrRefreshTooSoon) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Limits were read less than a minute ago")
		return
	} else if err != nil {
		refreshErr = err.Error()
	}
	s.writeHarnessUsage(w, r, refreshErr)
}

func (s *Server) writeHarnessUsage(w http.ResponseWriter, r *http.Request, refreshErr string) {
	list, err := s.usage.Store().List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := map[string]any{"harnesses": list}
	if refreshErr != "" {
		out["refresh_error"] = refreshErr
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load()
	states, _ := s.harness.States(r.Context())
	list := harness.List(cfg, tmux.HarnessLocator{})
	rows := make([]map[string]any, 0, len(list))
	for _, info := range list {
		rows = append(rows, s.harnessRow(r.Context(), cfg, info, states))
	}
	writeJSON(w, http.StatusOK, map[string]any{"harnesses": rows})
}

// harnessRow is one row of GET /v1/harnesses. PATCH returns the same shape.
func (s *Server) harnessRow(ctx context.Context, cfg *config.Config, info harness.Info, states map[string]harness.State) map[string]any {
	row := map[string]any{
		"name":           info.Name,
		"label":          info.Label,
		"vendor":         info.Vendor,
		"home":           info.Home,
		"bin_path":       info.BinPath,
		"enabled":        info.Enabled,
		"installed":      info.Installed,
		"terminal_login": info.TerminalLogin,
		"installable":    info.Installable,
		"hidden":         info.Hidden,
		"auth_mode":      authMode(info.Name),
	}
	caps := s.harness.Caps(info.Name)
	row["login_modes"] = caps.LoginModes
	row["keychain_bound"] = caps.KeychainBound
	if st, ok := states[info.Name]; ok {
		row["auth_ok"] = st.AuthOK.Valid && st.AuthOK.Int64 == 1
		row["auth_status"] = st.Status
		row["auth_detail"] = st.Detail
		row["auth_account"] = st.Account
		row["auth_probed_at"] = emptyNil(st.ProbedAt)
		row["auth_expires_at"] = emptyNil(st.ExpiresAt)
	} else {
		row["auth_ok"] = nil
		row["auth_status"] = nil
		row["auth_probed_at"] = nil
		row["auth_expires_at"] = nil
	}
	model, effort := "", ""
	if cfg != nil {
		if p := cfg.Tools.Pin(info.Name); p != nil {
			model, effort = p.Model, p.Effort
		}
	}
	row["model"] = model
	row["effort"] = effort
	row["approval"] = cfg.HarnessApproval(info.Name)
	row["asks_approval"] = config.HarnessAsksApproval(info.Name)
	row["effort_levels"] = config.HarnessEffortLevels(info.Name)
	row["own_files"] = harness.OwnCount(info.Name, cfg)
	s.versionRow(ctx, cfg, info.Name, row)
	return row
}

// harnessRowFor builds the row of one harness after a change.
func (s *Server) harnessRowFor(ctx context.Context, name string) map[string]any {
	cfg, _ := config.Load()
	states, _ := s.harness.States(ctx)
	for _, info := range harness.List(cfg, tmux.HarnessLocator{}) {
		if info.Name == name {
			return s.harnessRow(ctx, cfg, info, states)
		}
	}
	return nil
}

// harnessOffError is the job validation text for a harness that is off. An
// empty name is the default harness.
func harnessOffError(name string) string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	if name == "" {
		name = cfg.Defaults.Command
	}
	if !harness.Known(name) || cfg.HarnessEnabled(name) {
		return ""
	}
	return harness.OffMessage(name)
}

func (s *Server) handleHarnessEnsure(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusBadRequest, "validation", "unknown harness")
		return
	}
	dir := home.HarnessDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tool, ok := tmux.ByName(name)
	if ok {
		if life := tool.Lifecycle(); life != nil {
			if err := life.Install(config.BaseDir(), config.BinDir()); err != nil {
				writeJSON(w, http.StatusOK, map[string]any{
					"name": name, "home": dir, "ensured": false, "error": err.Error(),
				})
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "home": dir, "ensured": true})
}

// handleHarnessModels lists the models of a harness: the CLI list (cli
// source) or the list trebi ships (static source). Else it answers none, so
// the UI keeps a free-text input.
func (s *Server) handleHarnessModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	writeJSON(w, http.StatusOK, tmux.ListModels(r.Context(), name))
}

// handleHarnessProviders lists the OpenCode model providers and which of them
// have a login.
func (s *Server) handleHarnessProviders(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	if name != tmux.OpenCode && name != tmux.OpenCode2 {
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "only OpenCode has providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": tmux.OpenCodeProviders(name)})
}

func (s *Server) handleHarnessAuthGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var st harness.AuthStatus
	if r.URL.Query().Get("live") == "1" {
		got, err := s.harness.Probe(r.Context(), name, harness.SourceProbe)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		st = got
	} else {
		row, err := s.harness.GetState(r.Context(), name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		st = stateToStatus(name, row)
	}
	caps := s.harness.Caps(name)
	writeJSON(w, http.StatusOK, authWire(st, caps))
}

func (s *Server) handleHarnessPatch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var body harness.Patch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.harness.Apply(r.Context(), name, body); err != nil {
		switch {
		case errors.Is(err, harness.ErrDefaultOff):
			writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "Choose another default harness first.")
		case errors.Is(err, harness.ErrNoConfigField):
			writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", name+" has no settings in config.yaml")
		default:
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return
	}
	if body.AuthMode != nil {
		s.invalidateDetect(name)
	}
	if err := s.applyVersionPatch(r.Context(), name, body); err != nil {
		writeVersionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.harnessRowFor(r.Context(), name))
}

func (s *Server) handleHarnessLogout(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var body struct {
		Mode    string `json:"mode"`
		Confirm bool   `json:"confirm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	st, err := s.harness.Logout(r.Context(), name, body.Mode, body.Confirm)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	s.invalidateDetect(name)
	writeJSON(w, http.StatusOK, authWire(st, s.harness.Caps(name)))
}

// invalidateDetect drops the cached login probe of one harness after a login
// or a logout, so the next picker or overview read sees the new state
// (plan 2026-09-26 H).
func (s *Server) invalidateDetect(name string) {
	s.runs.AgentwireRuntime().InvalidateDetect(agentwire.Harness(name))
}

func (s *Server) handleHarnessLoginPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var body struct {
		Token    string `json:"token"`
		Provider string `json:"provider"`
		Code     string `json:"code"`
		Mode     string `json:"mode"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)

	switch {
	case body.Token != "":
		st, err := s.harness.LoginToken(r.Context(), name, harness.TokenLogin{Token: body.Token, Provider: body.Provider})
		if err != nil && st.Status == "" {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		s.invalidateDetect(name)
		writeJSON(w, http.StatusOK, authWire(st, s.harness.Caps(name)))
	case body.Code != "":
		sess, err := s.harness.SubmitCode(r.Context(), name, body.Code)
		if err != nil {
			if errors.Is(err, harness.ErrNoLogin()) {
				writeErr(w, http.StatusConflict, "conflict", err.Error())
				return
			}
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sess)
	default:
		sess, err := s.harness.StartLogin(r.Context(), name, body.Mode, body.Provider)
		if err != nil {
			if caps, ok := harness.IsLocalOnly(err); ok {
				writeErrDetails(w, http.StatusUnprocessableEntity, "precondition_failed", err.Error(),
					harness.LocalOnlyDetails(name, caps))
				return
			}
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sess)
	}
}

func (s *Server) handleHarnessLoginGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	writeJSON(w, http.StatusOK, s.harness.GetLogin(name))
}

func (s *Server) handleHarnessLoginDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	s.harness.CancelLogin(name)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": name})
}

func (s *Server) handleHarnessLoginAttach(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	sess := s.harness.GetLogin(name)
	tmuxName := harness.LoginSessionName(name)
	if !sess.Active && sess.Phase == "" {
		writeErr(w, http.StatusConflict, "conflict", "no active login session")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	done := make(chan struct{})
	go s.attachInputPump(conn, tmuxName, done)
	s.attachScreenLoop(r.Context(), conn, tmuxName, done)
}

func authMode(name string) string {
	cfg, err := config.Load()
	if err != nil {
		return harness.AuthModeShared
	}
	return harness.AuthModeFromConfig(cfg, name)
}

func authWire(st harness.AuthStatus, caps harness.AuthCaps) map[string]any {
	out := map[string]any{
		"name":           st.Tool,
		"auth_ok":        st.OK(),
		"auth_status":    st.Status,
		"auth_mode":      st.Mode,
		"login_modes":    caps.LoginModes,
		"keychain_bound": caps.KeychainBound,
	}
	if st.Detail != "" {
		out["auth_detail"] = st.Detail
		out["detail"] = st.Detail
	}
	if st.Hint != "" {
		out["hint"] = st.Hint
	}
	if !st.ProbedAt.IsZero() {
		out["auth_probed_at"] = st.ProbedAt.UTC().Format(time.RFC3339Nano)
	}
	if st.ExpiresAt != nil && !st.ExpiresAt.IsZero() {
		out["auth_expires_at"] = st.ExpiresAt.UTC().Format(time.RFC3339Nano)
		out["expires_at"] = st.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func stateToStatus(name string, row harness.State) harness.AuthStatus {
	st := harness.AuthStatus{Tool: name, Mode: authMode(name), Status: row.Status}
	if st.Status == "" {
		if row.AuthOK.Valid && row.AuthOK.Int64 == 1 {
			st.Status = harness.AuthOK
		} else if row.AuthOK.Valid {
			st.Status = harness.AuthMissing
		} else {
			st.Status = harness.AuthUnknown
		}
	}
	st.Detail = row.Detail
	if t, err := time.Parse(time.RFC3339Nano, row.ProbedAt); err == nil {
		st.ProbedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, row.ExpiresAt); err == nil {
		st.ExpiresAt = &t
	}
	return st
}

func emptyNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
