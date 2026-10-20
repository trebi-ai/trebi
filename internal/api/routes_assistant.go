package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// routeAssistant mounts the assistant settings routes (plan F.1). The
// session routes ride on the conversation paths and register in
// routeConversations.
func (s *Server) routeAssistant(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/assistant", s.handleAssistantDefaultsGet)
	mux.HandleFunc("PUT /v1/assistant", s.handleAssistantDefaultsPut)
	mux.HandleFunc("POST /v1/assistant/native/test", s.handleAssistantNativeTest)
	mux.HandleFunc("GET /v1/workspaces/{id}/assistant", s.handleWorkspaceAssistantGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/assistant", s.handleWorkspaceAssistantPut)
	mux.HandleFunc("GET /v1/workspaces/{id}/assistant/preview", s.handleWorkspaceAssistantPreview)
}

// conversationRow is one conversation payload with the assistant session
// attached when the conversation is an assistant chat (plan F.2).
type conversationRow struct {
	*conversations.Conversation
	Session *assistant.SessionInfo `json:"session,omitempty"`
}

// assistantRow decorates one conversation with its session record.
func (s *Server) assistantRow(ctx context.Context, c *conversations.Conversation) conversationRow {
	row := conversationRow{Conversation: c}
	if c.OperatorID != "" || s.asst == nil {
		return row
	}
	if rec, err := s.asst.Session(ctx, c.ID); err == nil {
		info := rec.Info(s.asst.Live(c.ID))
		row.Session = &info
	}
	return row
}

// --- Settings (plan F.1) ---

func (s *Server) handleAssistantDefaultsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.asstResolve.Defaults(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":  settings,
		"harnesses": s.asstResolve.HarnessOptions(r.Context()),
	})
}

func (s *Server) handleAssistantDefaultsPut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) || rejectAnyToken(w, r) {
		return
	}
	var in assistant.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "config unavailable")
		return
	}
	if err := s.validateAssistantSettings(r.Context(), in, cfg.Assistant.SkillsOrDefault()); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	cfg.Assistant.Harness = strings.TrimSpace(in.Harness)
	cfg.Assistant.Model = in.Model
	cfg.Assistant.Effort = strings.TrimSpace(in.Effort)
	cfg.Assistant.Envset = strings.TrimSpace(in.Envset)
	cfg.Assistant.Mode = strings.TrimSpace(in.Mode)
	cfg.Assistant.Skills = in.Skills
	cfg.Assistant.Context = in.Context
	if in.Native != nil {
		if err := applyNativeSettings(&cfg.Assistant, in.Native); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	if err := cfg.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.appendBus(r.Context(), store.BusEvent{Topic: "config.updated", Action: "updated"})
	s.kickDoctor()
	settings, _ := s.asstResolve.Defaults(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":  settings,
		"harnesses": s.asstResolve.HarnessOptions(r.Context()),
	})
}

// validateNativeFormat checks api_format with the rule of applyNativeSettings.
func validateNativeFormat(f string) error {
	switch f {
	case "", "anthropic", "openai":
		return nil
	default:
		return fmt.Errorf("native.api_format %q not in [anthropic, openai]", f)
	}
}

// applyNativeSettings writes the provider block of a Defaults PUT into the
// config (plan 2026-09-26 G). A non-empty key goes to the pending-key slot;
// Save moves it into the secrets store and never into YAML.
func applyNativeSettings(a *config.AssistantConfig, in *assistant.NativeSettings) error {
	if err := validateNativeFormat(strings.TrimSpace(in.APIFormat)); err != nil {
		return err
	}
	a.Native.APIFormat = strings.TrimSpace(in.APIFormat)
	a.Native.BaseURL = strings.TrimSpace(in.BaseURL)
	// The model lives in the native block (annotation 2026-09-26): a save
	// under another harness carries the block unchanged, so a configured
	// native setup survives it. Empty means inherit assistant.model.
	a.Native.Model = strings.TrimSpace(in.Model)
	// api_key_env is read-only on /v1 (plan 2026-09-26 A): the PUT keeps the
	// config value, so a UI save never clears the YAML setting.
	a.Native.Headers = in.Headers
	a.Native.WebSearch = in.WebSearch
	a.Native.MaxTokens = in.MaxTokens
	a.Native.ContextTokens = in.ContextTokens
	if key := strings.TrimSpace(in.APIKey); key != "" {
		a.SetPendingKey(key)
	}
	return nil
}

// handleAssistantNativeTest runs one small model call with the unsaved
// native form values (plan 2026-09-26 B). It saves nothing and never logs
// the request body.
func (s *Server) handleAssistantNativeTest(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) || rejectAnyToken(w, r) {
		return
	}
	var in assistant.NativeTestRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := validateNativeFormat(strings.TrimSpace(in.Native.APIFormat)); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if s.nativeProbe == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "native probe unavailable")
		return
	}
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "config unavailable")
		return
	}
	// The form values overlay the live block; APIKeyPresent and APIKeyEnv
	// stay from the config, so an empty typed key resolves the stored one.
	overlay := cfg.Assistant.Native
	overlay.APIFormat = strings.TrimSpace(in.Native.APIFormat)
	overlay.BaseURL = strings.TrimSpace(in.Native.BaseURL)
	overlay.Headers = in.Native.Headers
	result := s.nativeProbe.Run(r.Context(), overlay,
		strings.TrimSpace(in.Model), strings.TrimSpace(in.Native.APIKey))
	writeJSON(w, http.StatusOK, result)
}

// validateAssistantSettings checks mode, harness, effort, and that every
// added skill exists (plan F.1). Skills in current stay valid, so a
// read-modify-write of the defaults never fails on an unchanged list.
func (s *Server) validateAssistantSettings(ctx context.Context, in assistant.Settings, current []string) error {
	if m := strings.TrimSpace(in.Mode); m != "" && m != assistant.ModeAsk &&
		m != assistant.ModeAutoEdit && m != assistant.ModeAuto {
		return fmt.Errorf("mode %q not in [ask, auto_edit, auto]", in.Mode)
	}
	if h := strings.TrimSpace(in.Harness); h != "" && !assistant.KnownHarness(h) {
		return fmt.Errorf("harness %q is not a known harness", in.Harness)
	}
	switch strings.TrimSpace(in.Effort) {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("effort %q not in [low, medium, high]", in.Effort)
	}
	if len(in.Skills) == 0 {
		return nil
	}
	list, err := s.skills.List(ctx, skills.ListOpts{Scope: skills.ScopeGlobal})
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, name := range current {
		known[name] = true
	}
	for _, sk := range list {
		known[sk.Name] = true
	}
	for _, name := range in.Skills {
		if !known[name] {
			return fmt.Errorf("unknown skill %q", name)
		}
	}
	return nil
}

func (s *Server) handleWorkspaceAssistantGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	if !s.assistantScoped(w, r, ws.ID) {
		return
	}
	eff, err := s.asstResolve.Effective(r.Context(), ws.ID)
	if err != nil {
		writeAssistantErr(w, err)
		return
	}
	ov, hash, err := s.ws.AssistantOverride(r.Context(), ws.ID)
	if err != nil {
		writeAssistantErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"effective": eff, "override": ov, "hash": hash,
	})
}

func (s *Server) handleWorkspaceAssistantPut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	if !s.assistantScoped(w, r, ws.ID) {
		return
	}
	var body struct {
		Override *assistant.Override `json:"override"`
		// Native is rejected: the provider is a Defaults-scope setting
		// (plan 2026-09-26 G).
		Native any `json:"native"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Override == nil {
		writeErr(w, http.StatusBadRequest, "validation", "body must carry an assistant override object")
		return
	}
	if body.Native != nil {
		writeErr(w, http.StatusBadRequest, "validation", "native provider is set in Defaults only")
		return
	}
	hash, err := s.ws.SetAssistantOverride(r.Context(), ws.ID, *body.Override, ifMatchHeader(r))
	if errors.Is(err, workspaces.ErrPrecondition) {
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", "trebi.yaml changed; reload and retry")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	eff, err := s.asstResolve.Effective(r.Context(), ws.ID)
	if err != nil {
		writeAssistantErr(w, err)
		return
	}
	setETagHash(w, hash)
	writeJSON(w, http.StatusOK, map[string]any{
		"effective": eff, "override": body.Override, "hash": hash,
	})
}

func (s *Server) handleWorkspaceAssistantPreview(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	if !s.assistantScoped(w, r, ws.ID) {
		return
	}
	instructions, err := s.asst.Preview(r.Context(), ws.ID)
	if err != nil {
		writeAssistantErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instructions": instructions})
}

// --- Sessions (plan F.2) ---

func (s *Server) handleConversationEvents(w http.ResponseWriter, r *http.Request) {
	conv, err := s.conv.Get(r.Context(), r.PathValue("id"))
	if s.convErr(w, err, "conversation not found") {
		return
	}
	if conv.OperatorID != "" {
		writeErr(w, http.StatusBadRequest, "validation", "agent conversations have no assistant events")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("follow") == "1" {
		s.streamAssistantEvents(w, r, conv.ID, after)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}
	events, err := s.asst.Events(r.Context(), conv.ID, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"events": events}
	if len(events) == limit {
		resp["next_after"] = events[len(events)-1].Seq
	}
	writeJSON(w, http.StatusOK, resp)
}

// streamAssistantEvents replays the stored events after after, then follows
// the live stream. One frame per event, one ": ping" every 15s (plan F.2).
func (s *Server) streamAssistantEvents(w http.ResponseWriter, r *http.Request, convID string, after int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	// Subscribe before the replay: a live event that also lands in the
	// replay window is dropped by its sequence number.
	sub, cancel := s.asst.Subscribe(convID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(e assistant.Event) bool {
		b, err := json.Marshal(e)
		if err != nil {
			return true
		}
		if _, werr := fmt.Fprintf(w, "event: assistant\ndata: %s\n\n", b); werr != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	last := after
	for {
		events, err := s.asst.Events(r.Context(), convID, last, 500)
		if err != nil {
			break
		}
		for _, e := range events {
			if !write(e) {
				return
			}
			if e.Seq > last {
				last = e.Seq
			}
		}
		if len(events) < 500 {
			break
		}
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-sub:
			if !ok {
				return
			}
			if e.Seq != 0 {
				if e.Seq <= last {
					continue // the replay already carried it
				}
				last = e.Seq
			}
			if !write(e) {
				return
			}
		}
	}
}

func (s *Server) handleConversationInterrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.asst.Interrupt(r.Context(), r.PathValue("id")); err != nil {
		writeAssistantErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationPermission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	err := s.asst.Answer(r.Context(), r.PathValue("id"), r.PathValue("pid"),
		assistant.Decision{Choice: body.Decision, Message: body.Message})
	if err != nil {
		writeAssistantErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.asst.SetMode(r.Context(), r.PathValue("id"), strings.TrimSpace(body.Mode)); err != nil {
		writeAssistantErr(w, err)
		return
	}
	// agent-wire has no live mode setter (plan N); the next start applies it.
	w.Header().Set("X-Trebi-Applies", "next_session")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationClose(w http.ResponseWriter, r *http.Request) {
	if err := s.asst.Close(r.Context(), r.PathValue("id")); err != nil {
		writeAssistantErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.asst.Delete(r.Context(), r.PathValue("id")); err != nil {
		if s.convErr(w, err, "conversation not found") {
			return
		}
		writeAssistantErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationRevert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn int    `json:"turn"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Turn <= 0 || strings.TrimSpace(body.Path) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "turn and path are required")
		return
	}
	info, err := s.asst.Revert(r.Context(), r.PathValue("id"), body.Turn, body.Path)
	if err != nil {
		writeAssistantErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// writeAssistantErr maps one manager error onto the HTTP envelope.
func writeAssistantErr(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
	case errors.Is(err, assistant.ErrValidation),
		errors.Is(err, assistant.ErrDecisionInvalid):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, assistant.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, assistant.ErrConflict),
		errors.Is(err, assistant.ErrNoLive):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, assistant.ErrPermissionUnknown),
		errors.Is(err, assistant.ErrNoSnapshot),
		errors.Is(err, assistant.ErrNoSession),
		errors.Is(err, conversations.ErrNotFound),
		errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, assistant.ErrUnavailable):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// rejectAnyToken refuses every bearer actor: some settings are human-only.
func rejectAnyToken(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := tools.ActorFrom(r.Context()); ok {
		writeForbidden(w, "this setting is human-only")
		return true
	}
	return false
}
