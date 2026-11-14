package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/agentcli"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/onboarding"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeOnboarding(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/onboarding", s.handleOnboardingGet)
	mux.HandleFunc("PATCH /v1/onboarding", s.handleOnboardingPatch)
	mux.HandleFunc("POST /v1/onboarding/defaults", s.handleOnboardingDefaults)
	mux.HandleFunc("POST /v1/onboarding/restart", s.handleOnboardingRestart)
}

// BindOnboarding swaps in the daemon's onboarding service.
func (s *Server) BindOnboarding(o *onboarding.Service) { bind(&s.onboarding, o) }

func (s *Server) handleOnboardingGet(w http.ResponseWriter, r *http.Request) {
	st, err := s.onboarding.Get(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleOnboardingPatch(w http.ResponseWriter, r *http.Request) {
	var p onboarding.Patch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	st, err := s.onboarding.Update(r.Context(), p)
	if err != nil {
		writeOnboardingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleOnboardingRestart(w http.ResponseWriter, r *http.Request) {
	st, err := s.onboarding.Restart(r.Context())
	if err != nil {
		writeOnboardingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleOnboardingDefaults(w http.ResponseWriter, r *http.Request) {
	d, err := s.onboarding.ApplyDefaults(r.Context(), s.ws, onboardingHarness{s.harness})
	if err != nil {
		writeOnboardingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func writeOnboardingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, onboarding.ErrRevision):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, onboarding.ErrInvalid), errors.Is(err, workspaces.ErrHandle):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// onboardingHarness is the harness side of the onboarding defaults.
type onboardingHarness struct{ svc *harness.Service }

// LoggedIn lists the visible harnesses with a working login. A harness with
// no stored probe is probed once.
func (h onboardingHarness) LoggedIn(ctx context.Context) ([]string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	states, err := h.svc.States(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, info := range harness.List(cfg, agentcli.HarnessLocator{}) {
		if info.Hidden || info.Base != "" {
			continue
		}
		ok := false
		if st, found := states[info.Name]; found {
			ok = st.AuthOK.Valid && st.AuthOK.Int64 == 1
		} else if info.Installed || info.Name == agentcli.Fake {
			au, err := h.svc.Probe(ctx, info.Name, harness.SourceProbe)
			ok = err == nil && au.OK()
		}
		if ok {
			out = append(out, info.Name)
		}
	}
	return out, nil
}

// TurnOn turns a harness on with the shared login. A harness that is on, or
// that has no setting, stays as it is.
func (h onboardingHarness) TurnOn(ctx context.Context, name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.HarnessEnabled(name) {
		return nil
	}
	on, mode := true, harness.AuthModeShared
	err = h.svc.Apply(ctx, name, harness.Patch{Enabled: &on, AuthMode: &mode})
	if errors.Is(err, harness.ErrNoConfigField) {
		return nil
	}
	return err
}
