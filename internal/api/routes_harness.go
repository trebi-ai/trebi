package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/agentcli"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeHarness(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/harnesses", s.handleHarnesses)
	mux.HandleFunc("POST /v1/harnesses", s.handleHarnessCreate)
	mux.HandleFunc("DELETE /v1/harnesses/{name}", s.handleHarnessDelete)
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
	mux.HandleFunc("GET /v1/harnesses/{name}/hooks", s.handleHarnessHooks)
	mux.HandleFunc("PUT /v1/harnesses/{name}/hooks", s.handleHarnessHooks)
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
	list := harness.List(cfg, agentcli.HarnessLocator{})
	jobs := s.harnessJobCounts(r.Context())
	rows := make([]map[string]any, 0, len(list))
	for _, info := range list {
		rows = append(rows, s.harnessRow(r.Context(), cfg, info, states, jobs))
	}
	writeJSON(w, http.StatusOK, map[string]any{"harnesses": rows})
}

// harnessRow is one row of GET /v1/harnesses. PATCH returns the same shape.
func (s *Server) harnessRow(ctx context.Context, cfg *config.Config, info harness.Info, states map[string]harness.State, jobs map[string]int) map[string]any {
	row := map[string]any{
		"name":           info.Name,
		"label":          info.Label,
		"vendor":         info.Vendor,
		"base":           emptyNil(info.Base),
		"env":            harness.Env(cfg, info.Name),
		"sign_in":        emptyNil(harness.SignIn(cfg, info.Name)),
		"jobs":           jobs[info.Name],
		"home":           info.Home,
		"bin_path":       info.BinPath,
		"enabled":        info.Enabled,
		"installed":      info.Installed,
		"terminal_login": info.TerminalLogin,
		"installable":    info.Installable,
		"hidden":         info.Hidden,
		"auth_mode":      authMode(info.Name),
	}
	kind := cmp.Or(info.Base, info.Name)
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
	row["pause_at"] = cfg.HarnessPauseAt(info.Name)
	row["asks_approval"] = config.HarnessAsksApproval(kind)
	row["effort_levels"] = config.HarnessEffortLevels(kind)
	row["own_files"] = s.harness.OwnCount(ctx, info.Name)
	s.versionRow(ctx, cfg, info.Name, row)
	return row
}

// harnessRowFor builds the row of one harness after a change.
func (s *Server) harnessRowFor(ctx context.Context, name string) map[string]any {
	cfg, _ := config.Load()
	states, _ := s.harness.States(ctx)
	for _, info := range harness.List(cfg, agentcli.HarnessLocator{}) {
		if info.Name == name {
			return s.harnessRow(ctx, cfg, info, states, s.harnessJobCounts(ctx))
		}
	}
	return nil
}

// harnessJobs lists the jobs and the agents that name harness in their file.
func (s *Server) harnessJobs(ctx context.Context) (map[string][]workspaces.JobRow, error) {
	rows, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string][]workspaces.JobRow{}
	for _, row := range rows {
		if row.Harness != "" && (row.Kind == "job" || row.Kind == "agent") {
			out[row.Harness] = append(out[row.Harness], row)
		}
	}
	return out, nil
}

func (s *Server) harnessJobCounts(ctx context.Context) map[string]int {
	byName, err := s.harnessJobs(ctx)
	if err != nil {
		s.log.Warn("count the jobs of each harness", "error", err)
	}
	out := make(map[string]int, len(byName))
	for name, rows := range byName {
		out[name] = len(rows)
	}
	return out
}

// handleHarnessCreate makes a variant: a harness that runs the program of
// its base with its own home, login, and env.
func (s *Server) handleHarnessCreate(w http.ResponseWriter, r *http.Request) {
	var body harness.Variant
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.harness.CreateVariant(r.Context(), body); err != nil {
		writeHarnessErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.harnessRowFor(r.Context(), body.Name))
}

// handleHarnessDelete removes a variant. With move_to, its jobs, the
// default, and the assistant move to that harness first. Without it, a
// variant in use is refused with in_use.
func (s *Server) handleHarnessDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !harness.IsVariant(cfg, name) {
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", harness.ErrNotVariant.Error())
		return
	}
	byName, err := s.harnessJobs(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	jobs := byName[name]
	inUse := len(jobs) > 0 || cfg.Defaults.Command == name || cfg.Assistant.Harness == name
	moveTo := strings.TrimSpace(r.URL.Query().Get("move_to"))
	if moveTo == "" && inUse {
		writeErrDetails(w, http.StatusConflict, "in_use", "Jobs, the default, or the assistant use this harness. Choose a harness for them.", map[string]any{"jobs": len(jobs)})
		return
	}
	if moveTo != "" {
		if moveTo == name || !harness.Known(moveTo) {
			writeErr(w, http.StatusBadRequest, "validation", "move_to must name another harness")
			return
		}
		if err := s.moveHarnessUsers(r.Context(), cfg, name, moveTo, jobs); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	if err := s.harness.DeleteVariant(r.Context(), name); err != nil {
		writeHarnessErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveHarnessUsers points the jobs, the default, and the assistant that use
// from to the harness to.
func (s *Server) moveHarnessUsers(ctx context.Context, cfg *config.Config, from, to string, jobs []workspaces.JobRow) error {
	touched := map[string]bool{}
	for _, row := range jobs {
		if err := job.MergePatchFrontmatter(row.Path, map[string]any{"harness": to}); err != nil {
			return fmt.Errorf("move %s: %w", row.Name, err)
		}
		touched[row.WorkspaceID] = true
	}
	for id := range touched {
		ws, err := s.ws.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := s.ws.RebuildIndex(ctx, ws); err != nil {
			return err
		}
	}
	changed := false
	if cfg.Defaults.Command == from {
		if err := cfg.SetPath("defaults.command", to); err != nil {
			return err
		}
		changed = true
	}
	if cfg.Assistant.Harness == from {
		if err := cfg.SetPath("assistant.harness", to); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return cfg.Save()
}

// writeHarnessErr maps a harness service error to a status.
func writeHarnessErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, harness.ErrVariantExists):
		writeErr(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, harness.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, harness.ErrNotVariant), errors.Is(err, harness.ErrVariantProgram):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", err.Error())
	case errors.Is(err, harness.ErrDefaultOff):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "Choose another default harness first.")
	case errors.Is(err, harness.ErrNoConfigField):
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", err.Error())
	default:
		writeVisorErr(w, err)
	}
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
	tool, ok := agentcli.ByName(name)
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
	cfg, _ := config.Load()
	list := agentcli.ListModels(r.Context(), cfg.HarnessBase(name))
	// A variant on another service names its model in its env.
	if p := cfg.Tools.Pin(name); p != nil && cfg.HarnessBase(name) == agentcli.Claude && p.Env["ANTHROPIC_MODEL"] != "" && !strings.HasPrefix(p.Env["ANTHROPIC_MODEL"], "secret:") {
		m := p.Env["ANTHROPIC_MODEL"]
		list.Default = m
		for i := range list.Models {
			list.Models[i].Default = list.Models[i].ID == m
		}
		if !slices.ContainsFunc(list.Models, func(i agentcli.ModelInfo) bool { return i.ID == m }) {
			list.Models = append([]agentcli.ModelInfo{{ID: m, Label: m, Default: true}}, list.Models...)
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleHarnessProviders lists the OpenCode model providers and which of them
// have a login.
func (s *Server) handleHarnessProviders(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	if name != agentcli.OpenCode && name != agentcli.OpenCode2 {
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed", "only OpenCode has providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": agentcli.OpenCodeProviders(name)})
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
		writeHarnessErr(w, err)
		return
	}
	if body.AuthMode != nil {
		s.invalidateDetect(name)
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

// handleHarnessHooks reads, or with PUT {"on": bool} sets, the hooks that
// show the sessions of a harness that start outside Trebi.
func (s *Server) handleHarnessHooks(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var state string
	var err error
	if r.Method == http.MethodPut {
		var body struct {
			On *bool `json:"on"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.On == nil {
			writeErr(w, http.StatusBadRequest, "validation", "on is required")
			return
		}
		state, err = s.observed().SetHooks(r.Context(), name, *body.On)
	} else {
		state, err = s.observed().Hooks(r.Context(), name)
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"harness": name, "state": state})
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
			writeVisorErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sess)
	default:
		sess, err := s.harness.StartLogin(r.Context(), name, body.Mode, body.Provider)
		if caps, ok := harness.IsLocalOnly(err); ok {
			writeErrDetails(w, http.StatusUnprocessableEntity, "precondition_failed", err.Error(),
				harness.LocalOnlyDetails(name, caps))
			return
		}
		if err != nil {
			writeVisorErr(w, err)
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
	writeJSON(w, http.StatusOK, s.harness.GetLogin(r.Context(), name))
}

func (s *Server) handleHarnessLoginDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	if err := s.harness.CancelLogin(r.Context(), name); err != nil && !errors.Is(err, agentvisor.ErrNotFound) {
		writeVisorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": name})
}

func (s *Server) handleHarnessLoginAttach(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	sess := s.harness.GetLogin(r.Context(), name)
	if !sess.Active || sess.Session == "" {
		writeErr(w, http.StatusConflict, "conflict", "no active login session")
		return
	}
	pane := sess.Session
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	done := make(chan struct{})
	go s.attachInputPump(r.Context(), conn, pane, done)
	s.attachScreenLoop(r.Context(), conn, pane, done)
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
