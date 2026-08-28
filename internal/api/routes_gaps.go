package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// handleRunAttach streams terminal bytes over WebSocket (D2).
// Protocol: client may send {"type":"input","data":"..."} or {"type":"take_control"}.
// Server sends {"type":"screen","data":"..."}.
func (s *Server) handleRunAttach(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if run.TmuxSession == "" {
		writeErr(w, http.StatusBadRequest, "validation", "no tmux session")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	done := make(chan struct{})
	go s.attachInputPump(conn, run.TmuxSession, done)
	s.attachScreenLoop(r.Context(), conn, run.TmuxSession, done)
}

// attachInputPump forwards client input messages into tmux. It closes done
// when the client read fails (disconnect).
func (s *Server) attachInputPump(conn *websocket.Conn, session string, done chan<- struct{}) {
	defer close(done)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch fmt.Sprint(m["type"]) {
		case "input", "keys":
			_ = tmux.Send(session, fmt.Sprint(m["data"]))
		case "key":
			_ = tmux.SendKey(session, fmt.Sprint(m["data"]))
		case "take_control":
			// acknowledged — full TUI already live in tmux
		}
	}
}

// attachScreenLoop sends screen snapshots until the client disconnects or
// the tmux session ends.
func (s *Server) attachScreenLoop(ctx context.Context, conn *websocket.Conn, session string, done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			screen, err := tmux.ReadScreen(session)
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "error", "data": err.Error()})
				return
			}
			if err := conn.WriteJSON(map[string]any{"type": "screen", "data": screen}); err != nil {
				return
			}
			if !tmux.HasSession(session) {
				_ = conn.WriteJSON(map[string]any{"type": "ended"})
				return
			}
		}
	}
}

// handleMemoryDelete removes the whole memory file (reset to empty).
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request, assistant bool) {
	root, rel, path, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !s.requireIfMatch(w, r, root, rel) {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": path})
}

// handleMemoryEntriesPost appends one B22 ledger entry ([YYYY-MM-DD] text).
func (s *Server) handleMemoryEntriesPost(w http.ResponseWriter, r *http.Request, assistant bool) {
	var body struct {
		Date  string `json:"date"`
		Text  string `json:"text"`
		Entry string `json:"entry"` // legacy alias for text
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	text := body.Text
	if text == "" {
		text = body.Entry
	}
	if text == "" {
		writeErr(w, http.StatusBadRequest, "validation", "text required")
		return
	}
	root, rel, path, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !s.requireIfMatch(w, r, root, rel) {
		return
	}
	ent, err := job.AppendMemoryEntry(path, body.Date, text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	hash := s.fileHash(r.Context(), root, rel)
	setETagHash(w, hash)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": ent.ID, "date": ent.Date, "text": ent.Text,
		"path": path, "hash": hash,
	})
}

// handleMemoryEntriesDelete removes one ledger entry by id.
func (s *Server) handleMemoryEntriesDelete(w http.ResponseWriter, r *http.Request, assistant bool) {
	eid := r.PathValue("eid")
	if eid == "" {
		eid = r.PathValue("id")
	}
	root, rel, path, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !s.requireIfMatch(w, r, root, rel) {
		return
	}
	if err := job.RemoveMemoryEntry(path, eid); err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	hash := s.fileHash(r.Context(), root, rel)
	setETagHash(w, hash)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": eid, "hash": hash})
}

// memoryPath maps the request to the MEMORY.md sibling of the job file.
func (s *Server) memoryPath(r *http.Request, assistant bool) (string, error) {
	if assistant {
		wsID := r.PathValue("id")
		rows, err := s.ws.ListJobs(r.Context(), wsID)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			if row.Kind == "assistant" {
				return filepath.Join(filepath.Dir(row.Path), "MEMORY.md"), nil
			}
		}
		// assistant fallback: workspace-root MEMORY.md (TREBI.md sibling)
		ws, err := s.ws.Get(r.Context(), wsID)
		if err != nil {
			return "", errJobNotFound
		}
		return filepath.Join(ws.Path, "MEMORY.md"), nil
	}
	row, err := s.findJobRow(r.Context(), r.URL.Query().Get("workspace"), r.PathValue("name"))
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(row.Path), "MEMORY.md"), nil
}

func (s *Server) handleAssistantPut(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	path := filepath.Join(ws.Path, "TREBI.md")
	if body.Content == "" {
		body.Content = "---\nharness: claude\n---\n\nYou are the workspace assistant.\n"
	}
	info, _, err := s.files.Write(r.Context(), ws.Path, "TREBI.md", []byte(body.Content), ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	setETagHash(w, info.Hash)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "hash": info.Hash})
}

func (s *Server) handleAssistantPatch(w http.ResponseWriter, r *http.Request) {
	// patch disabled flag on TREBI.md
	wsID := r.PathValue("id")
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path := filepath.Join(ws.Path, "TREBI.md")
	if !s.requireIfMatch(w, r, ws.Path, "TREBI.md") {
		return
	}
	if body.Disabled != nil {
		_ = job.SetDisabled(path, *body.Disabled)
	}
	setETagHash(w, s.fileHash(r.Context(), ws.Path, "TREBI.md"))
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "patched": true})
}

func (s *Server) handleAssistantDelete(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	path := filepath.Join(ws.Path, "TREBI.md")
	if !s.requireIfMatch(w, r, ws.Path, "TREBI.md") {
		return
	}
	_ = os.Remove(path)
	_ = s.ws.RebuildIndex(r.Context(), ws)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": path})
}

func (s *Server) handleDoctorFix(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("finding_id")
	switch {
	case id == "config-missing":
		// Same documented template the daemon seeds on first start.
		if _, err := config.Seed(); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"fixed": id, "action": "wrote_default_config"})
	case strings.HasPrefix(id, "harness-"):
		name := strings.TrimPrefix(id, "harness-")
		if !harness.Known(name) {
			writeErr(w, http.StatusBadRequest, "validation", "unknown harness")
			return
		}
		_ = os.MkdirAll(home.HarnessDir(name), 0o755)
		writeJSON(w, http.StatusOK, map[string]any{"fixed": id, "action": "ensured_harness_home"})
	case id == "db-missing":
		writeJSON(w, http.StatusOK, map[string]any{"fixed": id, "action": "run_migrate_db", "hint": "trebi service migrate-db"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"fixed": false, "id": id, "note": "no automatic fix"})
	}
	s.log.Info("doctor.fix_applied", "id", id)
	_ = s.alerts.ResolveSubject(r.Context(), "doctor/"+id, "auto")
}
