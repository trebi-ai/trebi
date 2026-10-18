package api

// Structured MEMORY.md routes (B22 §3.3 rev 2). One handler set serves the
// three mounts: jobs (?workspace=), folder agents, and the workspace
// assistant.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// memoryScope names whose MEMORY.md a memory request targets.
type memoryScope int

const (
	memoryJob memoryScope = iota
	memoryAssistant
	memoryAgent
)

// owner is the ops-log owner tag for a scope.
func (s memoryScope) owner() string {
	switch s {
	case memoryAssistant:
		return "assistant"
	case memoryAgent:
		return "agent"
	default:
		return "job"
	}
}

// routeMemory mounts the memory route set on the three owner roots: jobs
// (?workspace=), folder agents, and the workspace assistant. The pattern set
// is identical per root.
func (s *Server) routeMemory(mux *http.ServeMux) {
	// Job scope.
	j := memoryJob
	mux.HandleFunc("GET /v1/jobs/{name}/memory", s.mem(j, s.handleMemoryGet))
	mux.HandleFunc("PUT /v1/jobs/{name}/memory", s.mem(j, s.handleMemoryPut))
	mux.HandleFunc("DELETE /v1/jobs/{name}/memory", s.mem(j, s.handleMemoryReset))
	mux.HandleFunc("POST /v1/jobs/{name}/memory/validate", s.mem(j, s.handleMemoryValidate))
	mux.HandleFunc("POST /v1/jobs/{name}/memory/migrate", s.mem(j, s.handleMemoryMigrate))
	mux.HandleFunc("PUT /v1/jobs/{name}/memory/context", s.memSection(j, "context"))
	mux.HandleFunc("POST /v1/jobs/{name}/memory/lessons", s.memEntry(j, "lessons"))
	mux.HandleFunc("DELETE /v1/jobs/{name}/memory/lessons/{eid}", s.memEntryDelete(j, "lessons"))
	mux.HandleFunc("POST /v1/jobs/{name}/memory/log", s.memEntry(j, "log"))
	mux.HandleFunc("DELETE /v1/jobs/{name}/memory/log/{eid}", s.memEntryDelete(j, "log"))

	// Agent scope.
	a := memoryAgent
	mux.HandleFunc("GET /v1/workspaces/{id}/agents/{name}/memory", s.mem(a, s.handleMemoryGet))
	mux.HandleFunc("PUT /v1/workspaces/{id}/agents/{name}/memory", s.mem(a, s.handleMemoryPut))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/agents/{name}/memory", s.mem(a, s.handleMemoryReset))
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/memory/validate", s.mem(a, s.handleMemoryValidate))
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/memory/migrate", s.mem(a, s.handleMemoryMigrate))
	mux.HandleFunc("PUT /v1/workspaces/{id}/agents/{name}/memory/context", s.memSection(a, "context"))
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/memory/lessons", s.memEntry(a, "lessons"))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/agents/{name}/memory/lessons/{eid}", s.memEntryDelete(a, "lessons"))
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/memory/log", s.memEntry(a, "log"))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/agents/{name}/memory/log/{eid}", s.memEntryDelete(a, "log"))

	// Assistant scope.
	w := memoryAssistant
	mux.HandleFunc("GET /v1/workspaces/{id}/assistant/memory", s.mem(w, s.handleMemoryGet))
	mux.HandleFunc("PUT /v1/workspaces/{id}/assistant/memory", s.mem(w, s.handleMemoryPut))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant/memory", s.mem(w, s.handleMemoryReset))
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/memory/validate", s.mem(w, s.handleMemoryValidate))
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/memory/migrate", s.mem(w, s.handleMemoryMigrate))
	mux.HandleFunc("PUT /v1/workspaces/{id}/assistant/memory/context", s.memSection(w, "context"))
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/memory/lessons", s.memEntry(w, "lessons"))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant/memory/lessons/{eid}", s.memEntryDelete(w, "lessons"))
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/memory/log", s.memEntry(w, "log"))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant/memory/log/{eid}", s.memEntryDelete(w, "log"))
}

// mem binds a scope-taking handler into an http.HandlerFunc.
func (s *Server) mem(sc memoryScope, h func(w http.ResponseWriter, r *http.Request, sc memoryScope)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h(w, r, sc) }
}

// memSection binds a section handler (context).
func (s *Server) memSection(sc memoryScope, section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleMemorySectionPut(w, r, sc, section) }
}

// memEntry binds an entry-add handler (lessons or log).
func (s *Server) memEntry(sc memoryScope, section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleMemoryEntryPost(w, r, sc, section) }
}

// memEntryDelete binds an entry-delete handler (lessons or log).
func (s *Server) memEntryDelete(sc memoryScope, section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleMemoryEntryDelete(w, r, sc, section) }
}

// memoryTarget is one resolved memory request: the workspace, the MEMORY.md
// placement, and the effective options after config defaults.
type memoryTarget struct {
	ws  *workspaces.Workspace
	rel string // MEMORY.md relative to ws.Path
	abs string
	opt memory.Options
}

// memoryTarget resolves scope, paths, and options. Jobs and agents take the
// options from the owner frontmatter; the assistant uses config defaults.
func (s *Server) memoryTarget(r *http.Request, scope memoryScope) (*memoryTarget, error) {
	def := config.DefaultMemory()
	if c, err := config.Load(); err == nil && c != nil {
		def = c.Defaults.Memory
	}
	switch scope {
	case memoryAssistant:
		wsID := r.PathValue("id")
		ws, err := s.ws.Get(r.Context(), wsID)
		if err != nil {
			return nil, err
		}
		// The assistant ledger sits at the workspace root.
		return s.newMemoryTarget(ws, ws.Path, job.MemoryConfig{}.Resolve(def))
	case memoryAgent:
		row, err := s.findAgentRow(r.Context(), r.PathValue("id"), r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		ws, err := s.ws.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return s.newMemoryTarget(ws, filepath.Dir(row.Path), s.ownerOptions(row.Path, def))
	default:
		row, err := s.findJobRow(r.Context(), r.URL.Query().Get("workspace"), r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		ws, err := s.ws.Get(r.Context(), row.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return s.newMemoryTarget(ws, filepath.Dir(row.Path), s.ownerOptions(row.Path, def))
	}
}

// newMemoryTarget places MEMORY.md next to the owner document.
func (s *Server) newMemoryTarget(ws *workspaces.Workspace, dir string, opt memory.Options) (*memoryTarget, error) {
	abs := filepath.Join(dir, "MEMORY.md")
	rel, err := workspaceRel(ws.Path, abs)
	if err != nil {
		return nil, err
	}
	return &memoryTarget{ws: ws, rel: rel, abs: abs, opt: opt}, nil
}

// ownerOptions reads the memory frontmatter of an owner document. A document
// that does not parse falls back to config defaults.
func (s *Server) ownerOptions(path string, def config.MemoryDefaults) memory.Options {
	if j, _ := job.ParseFile(path); j != nil {
		return j.MemoryOpts.Resolve(def)
	}
	return job.MemoryConfig{}.Resolve(def)
}

// memoryRead loads the file bytes. A missing file returns the skeleton with
// a zero time, so every handler sees a workable document.
func (s *Server) memoryRead(ctx context.Context, tgt *memoryTarget) (data []byte, at time.Time, found bool, err error) {
	data, info, err := s.files.Read(ctx, tgt.ws.Path, tgt.rel)
	if errors.Is(err, files.ErrNotFound) {
		return memory.Skeleton(), time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return data, info.MTime, true, nil
}

// memoryDoc reads the file into an editable Doc. Legacy content migrates
// first; a missing file starts from the skeleton. The second result reports
// that a legacy conversion happened.
func (s *Server) memoryDoc(ctx context.Context, tgt *memoryTarget) (*memory.Doc, bool, error) {
	data, _, _, err := s.memoryRead(ctx, tgt)
	if err != nil {
		return nil, false, err
	}
	doc, _ := memory.Parse(data)
	if !doc.Legacy {
		return doc, false, nil
	}
	doc, _ = memory.Parse(memory.Migrate(data))
	return doc, true, nil
}

// memoryWrite writes new file bytes with If-Match and sets the ETag. It
// returns false after writing the error response.
func (s *Server) memoryWrite(w http.ResponseWriter, r *http.Request, tgt *memoryTarget, data []byte) (files.Info, bool) {
	info, _, err := s.files.Write(r.Context(), tgt.ws.Path, tgt.rel, data, ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return files.Info{}, false
	}
	setETagHash(w, info.Hash)
	return info, true
}

// logMemory records one memory mutation in the ops log.
func (s *Server) logMemory(scope memoryScope, tgt *memoryTarget, event string) {
	s.log.Info(event, "owner", scope.owner(), "workspace", tgt.ws.ID, "path", tgt.rel)
}

// handleMemoryGet serves the full memory info payload. A missing file
// returns the skeleton content with size 0 and an empty doc.
func (s *Server) handleMemoryGet(w http.ResponseWriter, r *http.Request, scope memoryScope) {
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	data, at, found, err := s.memoryRead(r.Context(), tgt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	info := memory.Inspect(data, tgt.opt, at)
	if !found {
		info.SizeBytes = 0
		info.Hash = ""
	}
	writeJSON(w, http.StatusOK, info)
}

// handleMemoryPut replaces the whole file. Invalid content returns 422 with
// the issues unless force=1.
func (s *Server) handleMemoryPut(w http.ResponseWriter, r *http.Request, scope memoryScope) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	if !s.requireAgentMemoryScope(w, r, tgt) {
		return
	}
	if r.URL.Query().Get("force") != "1" {
		if issues := memory.Validate([]byte(body.Content), tgt.opt); len(issues) > 0 {
			writeErrDetails(w, http.StatusUnprocessableEntity, "validation",
				"MEMORY.md has issues; pass ?force=1 to write anyway", issues)
			return
		}
	}
	info, ok := s.memoryWrite(w, r, tgt, []byte(body.Content))
	if !ok {
		return
	}
	s.logMemory(scope, tgt, "memory.updated")
	writeJSON(w, http.StatusOK, map[string]any{"path": tgt.abs, "hash": info.Hash})
}

// handleMemoryReset rewrites the skeleton. The body must carry confirm.
func (s *Server) handleMemoryReset(w http.ResponseWriter, r *http.Request, scope memoryScope) {
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	if !s.requireAgentMemoryScope(w, r, tgt) {
		return
	}
	info, ok := s.memoryWrite(w, r, tgt, memory.Skeleton())
	if !ok {
		return
	}
	s.logMemory(scope, tgt, "memory.reset")
	writeJSON(w, http.StatusOK, map[string]any{"path": tgt.abs, "hash": info.Hash, "reset": true})
}

// handleMemoryValidate checks the body content, or the file on disk when the
// body is empty. It never writes.
func (s *Server) handleMemoryValidate(w http.ResponseWriter, r *http.Request, scope memoryScope) {
	var body struct {
		Content *string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	var data []byte
	if body.Content != nil {
		data = []byte(*body.Content)
	} else if data, _, _, err = s.memoryRead(r.Context(), tgt); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	doc, _ := memory.Parse(data)
	issues := memory.Validate(data, tgt.opt)
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": len(issues) == 0, "issues": issues, "legacy": doc.Legacy,
	})
}

// handleMemoryMigrate converts a legacy B22 ledger to v1 on disk.
func (s *Server) handleMemoryMigrate(w http.ResponseWriter, r *http.Request, scope memoryScope) {
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	if !s.requireAgentMemoryScope(w, r, tgt) {
		return
	}
	data, at, found, err := s.memoryRead(r.Context(), tgt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		if _, ok := s.memoryWrite(w, r, tgt, memory.Skeleton()); !ok {
			return
		}
		s.logMemory(scope, tgt, "memory.migrated")
		writeJSON(w, http.StatusOK, memory.Inspect(memory.Skeleton(), tgt.opt, time.Now().UTC()))
		return
	}
	doc, _ := memory.Parse(data)
	if !doc.Legacy {
		writeErr(w, http.StatusConflict, "conflict", "memory file is already v2")
		return
	}
	fixed := memory.Migrate(data)
	if _, ok := s.memoryWrite(w, r, tgt, fixed); !ok {
		return
	}
	s.logMemory(scope, tgt, "memory.migrated")
	writeJSON(w, http.StatusOK, memory.Inspect(fixed, tgt.opt, at))
}

// handleMemorySectionPut replaces the Context body. A legacy file
// migrates first.
func (s *Server) handleMemorySectionPut(w http.ResponseWriter, r *http.Request, scope memoryScope, section string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	if !s.requireAgentMemoryScope(w, r, tgt) {
		return
	}
	doc, migrated, err := s.memoryDoc(r.Context(), tgt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if section == "context" {
		doc.SetContext(body.Body)
	}
	info, ok := s.memoryWrite(w, r, tgt, doc.Marshal())
	if !ok {
		return
	}
	if migrated {
		s.logMemory(scope, tgt, "memory.migrated")
	}
	s.logMemory(scope, tgt, "memory."+section+"_set")
	writeJSON(w, http.StatusOK, map[string]any{"path": tgt.abs, "hash": info.Hash})
}

// handleMemoryEntryPost appends one Lesson or Log entry. A Log add that
// crosses the cap returns 422 log_over_cap and writes nothing.
func (s *Server) handleMemoryEntryPost(w http.ResponseWriter, r *http.Request, scope memoryScope, section string) {
	var body struct {
		Text string `json:"text"`
		At   string `json:"at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "validation", "text required")
		return
	}
	if body.At != "" && !memory.ValidStamp(body.At) {
		writeErr(w, http.StatusBadRequest, "validation",
			"at must be \"YYYY-MM-DD\" or \"YYYY-MM-DD HH:MM\"")
		return
	}
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	if !s.requireAgentMemoryScope(w, r, tgt) {
		return
	}
	doc, migrated, err := s.memoryDoc(r.Context(), tgt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var entry memory.Entry
	if section == "lessons" {
		entry = doc.AddLesson(body.At, body.Text)
	} else {
		entry = doc.AddLog(body.At, body.Text)
		for _, iss := range memory.Validate(doc.Marshal(), tgt.opt) {
			if iss.Code == "log_over_cap" {
				writeErr(w, http.StatusUnprocessableEntity, "log_over_cap", iss.Msg)
				return
			}
		}
	}
	info, ok := s.memoryWrite(w, r, tgt, doc.Marshal())
	if !ok {
		return
	}
	if migrated {
		s.logMemory(scope, tgt, "memory.migrated")
	}
	s.logMemory(scope, tgt, "memory."+strings.TrimSuffix(section, "s")+"_added")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": entry.ID, "at": entry.At, "text": entry.Text, "hash": info.Hash,
	})
}

// handleMemoryEntryDelete removes one Lesson or Log entry by id.
func (s *Server) handleMemoryEntryDelete(w http.ResponseWriter, r *http.Request, scope memoryScope, section string) {
	eid := r.PathValue("eid")
	tgt, err := s.memoryTarget(r, scope)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	if !s.requireAgentMemoryScope(w, r, tgt) {
		return
	}
	doc, migrated, err := s.memoryDoc(r.Context(), tgt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !doc.Remove(section, eid) {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("entry %s not found", eid))
		return
	}
	info, ok := s.memoryWrite(w, r, tgt, doc.Marshal())
	if !ok {
		return
	}
	if migrated {
		s.logMemory(scope, tgt, "memory.migrated")
	}
	s.logMemory(scope, tgt, "memory."+strings.TrimSuffix(section, "s")+"_removed")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": eid, "hash": info.Hash})
}

// seedMemory writes the MEMORY.md skeleton when the file is absent.
func (s *Server) seedMemory(ctx context.Context, ws *workspaces.Workspace, rel string) {
	if _, err := s.files.Stat(ctx, ws.Path, rel); err == nil {
		return
	}
	if _, _, err := s.files.Write(ctx, ws.Path, rel, memory.Skeleton(), ""); err != nil {
		s.log.Warn("seed memory", "rel", rel, "error", err)
	}
}
