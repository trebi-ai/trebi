package api

import (
	"encoding/json"
	"fmt"
	"net/http"

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
	mux.HandleFunc("POST /v1/skills/{name}/update", s.handleSkillUpdate)
	mux.HandleFunc("GET /v1/secrets", s.handleSecrets)
	mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSecretPut)
	mux.HandleFunc("DELETE /v1/secrets/{name}", s.handleSecretDelete)
	mux.HandleFunc("GET /v1/config", s.handleConfigGet)
	mux.HandleFunc("PATCH /v1/config", s.handleConfigPatch)
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
	if len(list) > limit {
		list = list[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": list})
}

func (s *Server) handleSkillsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		SkillMD    string `json:"skill_md"`
		Provenance string `json:"provenance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	prov := skills.Provenance(body.Provenance)
	if prov == "" {
		prov = skills.ProvenanceLocal
	}
	sk, err := s.skills.Create(r.Context(), body.Name, body.SkillMD, prov)
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
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) handleSkillDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.skills.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

func (s *Server) handleSkillUpdate(w http.ResponseWriter, r *http.Request) {
	// pull upstream — not implemented for local; return current
	sk, err := s.skills.Get(r.Context(), r.PathValue("name"), true)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": sk, "updated": false, "note": "local skills have no upstream"})
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
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

func (s *Server) handleSecretPut(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "entries": configEntries()})
}

func (s *Server) handleConfigPatch(w http.ResponseWriter, r *http.Request) {
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
	sets, _ := body["set"].(map[string]any)
	if sets == nil {
		sets = body
	}
	for k, v := range sets {
		if k == "set" {
			continue
		}
		if err := config.SetPath(cfg, k, fmt.Sprint(v)); err != nil {
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
	writeJSON(w, http.StatusOK, map[string]any{"config": m, "entries": configEntries()})
}

// configEntries lists every known key with effective value, source, and default.
// A read error yields no rows: the caller still gets the config map.
func configEntries() []config.PathEntry {
	cfg, declared, err := config.LoadWithDeclared()
	if err != nil {
		return nil
	}
	return config.ListPathEntries(cfg, declared)
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
