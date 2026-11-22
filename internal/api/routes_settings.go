package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

func (s *Server) routeSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/secrets", s.handleSecrets)
	mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSecretPut)
	mux.HandleFunc("DELETE /v1/secrets/{name}", s.handleSecretDelete)
	mux.HandleFunc("GET /v1/config", s.handleConfigGet)
	mux.HandleFunc("PATCH /v1/config", s.handleConfigPatch)
	mux.HandleFunc("GET /v1/config/validate", s.handleConfigValidate)
	mux.HandleFunc("POST /v1/config/validate", s.handleConfigValidateDraft)
	mux.HandleFunc("GET /v1/config/schema", s.handleConfigSchema)
	mux.HandleFunc("GET /v1/config/yaml", s.handleConfigYAML)
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	names, err := secrets.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	limit := parseLimit(r)
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	label := harnessLabeler()
	owners := map[string]secrets.Owner{}
	for _, n := range names {
		if o, ok := secrets.OwnerOf(n, label); ok {
			owners[n] = o
		}
	}
	resp := map[string]any{"secrets": names, "owners": owners}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// harnessLabeler gives the display name of a harness id: the config label,
// else the spec label, else the id.
func harnessLabeler() func(string) string {
	cfg, _ := config.Load() //nolint:errcheck // a nil config falls back to the spec label
	return func(id string) string {
		if l := cfg.HarnessLabel(id); l != "" {
			return l
		}
		if spec, ok := harness.SpecFor(id); ok {
			return spec.Label
		}
		return id
	}
}

// refuseOwnedSecret answers 409 for a user write to a name that Trebi writes.
func refuseOwnedSecret(w http.ResponseWriter, name string) bool {
	o, ok := secrets.OwnerOf(name, harnessLabeler())
	if ok {
		writeErr(w, http.StatusConflict, "conflict", name+" is managed by Trebi. Change it in "+o.Label+".")
	}
	return ok
}

func (s *Server) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := secrets.CheckUserName(name); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if refuseOwnedSecret(w, name) {
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := secrets.Set(name, body.Value); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "secret.updated", Entity: "secret", EntityID: name, Action: "updated",
	})
	writeJSON(w, http.StatusOK, map[string]any{"name": name})
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := secrets.CheckUserName(name); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if refuseOwnedSecret(w, name) {
		return
	}
	if err := secrets.Delete(name); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "secret.deleted", Entity: "secret", EntityID: name, Action: "deleted",
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	// Prefer structured config via config.
	cfg, err := loadConfigMap()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"config": map[string]any{}})
		return
	}
	// entries carries every known key with its source, default, and description
	// so a remote CLI renders the server truth instead of its own local file.
	redactAssistantConfig(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "entries": configEntries()})
}

// handleConfigValidate checks the daemon config. It answers 200 either way,
// with ok and the problems, so a client reads one shape.
func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := cfg.Validate(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleConfigValidateDraft checks {set, unset} or {yaml} against a copy of
// config.yaml. It writes nothing and answers 200 with ok and the errors.
func (s *Server) handleConfigValidateDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Set   map[string]any `json:"set"`
		Unset []string       `json:"unset"`
		YAML  *string        `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	d := config.Draft{Set: map[string]string{}, Unset: body.Unset, YAML: body.YAML}
	errs := []configFieldErr{}
	for k, v := range body.Set {
		val, err := configScalar(v)
		if err != nil {
			errs = append(errs, configFieldErr{Key: k, Message: err.Error()})
			continue
		}
		if !keepSecret(k, val) {
			d.Set[k] = val
		}
	}
	for _, fe := range d.Check() {
		errs = append(errs, configFieldErr{Key: fe.Field, Message: fe.Msg})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(errs) == 0, "errors": errs})
}

// configFieldErr is one problem of a config draft.
type configFieldErr struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

func (s *Server) handleConfigSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, config.NewSchema())
}

// handleConfigYAML returns config.yaml with secret values masked.
func (s *Server) handleConfigYAML(w http.ResponseWriter, r *http.Request) {
	text, hash, err := config.RedactedYAML()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": text, "hash": hash})
}

func (s *Server) handleConfigPatch(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	zoneBefore := cfg.User.TimeZoneOrLocal()
	// Accept { "set": { "defaults.command": "claude" } } or flat dotted keys
	if raw, ok := body["unset"].([]any); ok {
		for _, item := range raw {
			k, _ := item.(string)
			if k == "" {
				continue
			}
			if err := cfg.UnsetPath(k); err != nil {
				writeErr(w, http.StatusBadRequest, "validation", err.Error())
				return
			}
		}
	}
	sets, _ := body["set"].(map[string]any)
	if sets == nil {
		sets = body
	}
	for k, v := range sets {
		if k == "set" || k == "unset" {
			continue
		}
		val, err := configScalar(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "key "+k+": "+err.Error())
			return
		}
		if keepSecret(k, val) {
			continue
		}
		if err := cfg.SetPath(k, val); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	if err := cfg.Save(); err != nil {
		var ce *config.ConfigError
		if errors.As(err, &ce) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.appendBus(r.Context(), store.BusEvent{Topic: "config.updated", Action: "updated"})
	if cfg.User.TimeZoneOrLocal() != zoneBefore {
		if err := s.sched.RecomputeDefaultZone(r.Context()); err != nil {
			s.log.Warn("schedule.recompute_zone", "err", err)
		}
	}
	if s.cloud != nil {
		s.cloud.SetRemoteMCP(cfg.Cloud.RemoteMCPOn())
		s.cloud.SetMobile(cfg.Cloud.MobileOn())
	}
	s.kickDoctor()
	m, _ := loadConfigMap()
	redactAssistantConfig(m)
	writeJSON(w, http.StatusOK, map[string]any{"config": m, "entries": configEntries()})
}

func redactAssistantConfig(m map[string]any) {
	if m == nil {
		return
	}
	asst, _ := m["assistant"].(map[string]any)
	if asst == nil {
		return
	}
	delete(asst, "api_key")
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if cfg.Assistant.Native.APIKeyPresent {
		asst["api_key"] = "********"
	}
	asst["api_key_source"] = cfg.Assistant.Native.KeySourceName()
}

// keepSecret reports whether val is the mask of a secret key, so the stored
// value stays.
func keepSecret(key, val string) bool {
	k, ok := config.NewSchema().Key(key)
	return ok && k.Secret && val == config.SecretMask
}

// configScalar renders one JSON value as the text SetPath parses. An array or
// an object becomes JSON text, for keys such as notify.route.
func configScalar(v any) (string, error) {
	switch t := v.(type) {
	case []any, map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("value must be a string, number, or boolean, not %T", v)
	}
}

// configEntries lists every known key with effective value, source, and default.
// A read error yields no rows: the caller still gets the config map.
func configEntries() []config.PathEntry {
	cfg, declared, err := config.LoadWithDeclared()
	if err != nil {
		return nil
	}
	return cfg.PathEntries(declared)
}

func loadConfigMap() (map[string]any, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
