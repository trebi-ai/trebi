package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

func (s *Server) routeSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/skills", s.handleSkills)
	mux.HandleFunc("POST /v1/skills", s.handleSkillsCreate)
	mux.HandleFunc("GET /v1/skills/{name}", s.handleSkillGet)
	mux.HandleFunc("PUT /v1/skills/{name}", s.handleSkillPut)
	mux.HandleFunc("DELETE /v1/skills/{name}", s.handleSkillDelete)
	mux.HandleFunc("POST /v1/skills/{name}/update", s.handleSkillRefresh)
	mux.HandleFunc("POST /v1/skills/{name}/refresh", s.handleSkillRefresh)
	mux.HandleFunc("GET /v1/secrets", s.handleSecrets)
	mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSecretPut)
	mux.HandleFunc("DELETE /v1/secrets/{name}", s.handleSecretDelete)
	mux.HandleFunc("GET /v1/config", s.handleConfigGet)
	mux.HandleFunc("PATCH /v1/config", s.handleConfigPatch)
	mux.HandleFunc("GET /v1/config/validate", s.handleConfigValidate)
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	list, err := s.skills.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []skills.Skill{}
	}
	// Name-keyed collection: limit only, no ULID cursor.
	limit := parseLimit(r)
	truncated := len(list) > limit
	if truncated {
		list = list[:limit]
	}
	resp := map[string]any{"skills": list}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSkillsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string         `json:"name"`
		SkillMD    string         `json:"skill_md"`
		Provenance string         `json:"provenance"`
		Source     *skills.Source `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	var sk *skills.Skill
	var err error
	if body.Source != nil {
		sk, err = s.skills.CreateFromSource(r.Context(), body.Name, *body.Source)
	} else {
		prov := skills.Provenance(body.Provenance)
		if prov == "" {
			prov = skills.ProvenanceLocal
		}
		sk, err = s.skills.Create(r.Context(), body.Name, body.SkillMD, prov)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

func (s *Server) handleSkillGet(w http.ResponseWriter, r *http.Request) {
	sk, err := s.skills.Get(r.Context(), r.PathValue("name"), true)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) handleSkillPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SkillMD   string          `json:"skill_md"`
		Harnesses map[string]bool `json:"harnesses"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	ifMatch := r.Header.Get("If-Match")
	sk, err := s.skills.Update(r.Context(), r.PathValue("name"), body.SkillMD, ifMatch, body.Harnesses)
	if err != nil {
		if s.writeSkillErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) handleSkillDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.skills.Delete(r.Context(), r.PathValue("name")); err != nil {
		if s.writeSkillErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

// handleSkillRefresh pulls the skill source again. A skill with no source
// comes back unchanged, with updated false.
func (s *Server) handleSkillRefresh(w http.ResponseWriter, r *http.Request) {
	sk, updated, err := s.skills.Refresh(r.Context(), r.PathValue("name"))
	if os.IsNotExist(errors.Unwrap(err)) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		if s.writeSkillErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	resp := map[string]any{"skill": sk, "updated": updated}
	if sk.Source == nil {
		resp["note"] = "skill has no source"
	}
	writeJSON(w, http.StatusOK, resp)
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
	resp := map[string]any{"secrets": names}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name := r.PathValue("name")
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
		if err := cfg.SetPath(k, val); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	if err := cfg.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.appendBus(r.Context(), store.BusEvent{Topic: "config.updated", Action: "updated"})
	m, _ := loadConfigMap()
	redactAssistantConfig(m)
	writeJSON(w, http.StatusOK, map[string]any{"config": m, "entries": configEntries()})
}

func (s *Server) writeSkillErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, skills.ErrInherited):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, skills.ErrPrecondition):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	default:
		return false
	}
	return true
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
	if cfg.Assistant.APIKeyPresent {
		asst["api_key"] = "********"
	}
	asst["api_key_source"] = cfg.Assistant.KeySourceName()
}

// configScalar renders one JSON scalar as the text SetPath parses. An object
// or array is rejected: every config key holds a scalar.
func configScalar(v any) (string, error) {
	switch t := v.(type) {
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
