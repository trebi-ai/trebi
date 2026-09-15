package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/tmux"
)

func (s *Server) routeHarness(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/harnesses", s.handleHarnesses)
	mux.HandleFunc("PATCH /v1/harnesses/{name}", s.handleHarnessPatch)
	mux.HandleFunc("POST /v1/harnesses/{name}/ensure", s.handleHarnessEnsure)
	mux.HandleFunc("GET /v1/harnesses/{name}/auth", s.handleHarnessAuthGet)
	mux.HandleFunc("GET /v1/harnesses/{name}/models", s.handleHarnessModels)
	mux.HandleFunc("POST /v1/harnesses/{name}/login", s.handleHarnessLoginPost)
	mux.HandleFunc("GET /v1/harnesses/{name}/login", s.handleHarnessLoginGet)
	mux.HandleFunc("DELETE /v1/harnesses/{name}/login", s.handleHarnessLoginDelete)
	mux.HandleFunc("GET /v1/harnesses/{name}/login/attach", s.handleHarnessLoginAttach)
	mux.HandleFunc("POST /v1/harnesses/{name}/logout", s.handleHarnessLogout)
}

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	states, _ := s.harness.States(r.Context())
	list := harness.List()
	rows := make([]map[string]any, 0, len(list))
	for _, info := range list {
		row := map[string]any{
			"name":     info.Name,
			"home":     info.Home,
			"bin_path": info.BinPath,
			"present":  info.Present,
		}
		row["auth_mode"] = authMode(info.Name)
		caps := s.harness.Caps(info.Name)
		row["login_modes"] = caps.LoginModes
		row["keychain_bound"] = caps.KeychainBound
		if st, ok := states[info.Name]; ok {
			row["auth_ok"] = st.AuthOK.Valid && st.AuthOK.Int64 == 1
			row["auth_status"] = st.Status
			row["auth_detail"] = st.Detail
			row["auth_probed_at"] = emptyNil(st.ProbedAt)
			row["auth_expires_at"] = emptyNil(st.ExpiresAt)
		} else {
			row["auth_ok"] = nil
			row["auth_status"] = nil
			row["auth_probed_at"] = nil
			row["auth_expires_at"] = nil
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"harnesses": rows})
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

// handleHarnessModels lists the models a harness CLI reports (cli source) or
// answers none, so the UI keeps a free-text input.
func (s *Server) handleHarnessModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	writeJSON(w, http.StatusOK, tmux.ListModels(r.Context(), name))
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
	var body struct {
		AuthMode string `json:"auth_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if !config.IsFirstClass(name) {
		writeErr(w, http.StatusUnprocessableEntity, "precondition_failed",
			"auth_mode is not persisted for "+name+"; only first-class tools store a pin in config.yaml")
		return
	}
	st, err := s.harness.SetMode(r.Context(), name, body.AuthMode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, authWire(st, s.harness.Caps(name)))
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
	writeJSON(w, http.StatusOK, authWire(st, s.harness.Caps(name)))
}

func (s *Server) handleHarnessLoginPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !harness.Known(name) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown harness")
		return
	}
	var body struct {
		Token string `json:"token"`
		Code  string `json:"code"`
		Mode  string `json:"mode"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)

	switch {
	case body.Token != "":
		st, err := s.harness.LoginToken(r.Context(), name, body.Token)
		if err != nil && st.Status == "" {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
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
		sess, err := s.harness.StartLogin(r.Context(), name, body.Mode)
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
