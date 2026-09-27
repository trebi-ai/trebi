package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
)

//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeConnectors(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connectors", s.handleConnectors)
	mux.HandleFunc("POST /v1/connectors", s.handleConnectorsCreate)
	mux.HandleFunc("GET /v1/connectors/{name}", s.handleConnectorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}", s.handleConnectorPut)
	mux.HandleFunc("PATCH /v1/connectors/{name}", s.handleConnectorPatch)
	mux.HandleFunc("DELETE /v1/connectors/{name}", s.handleConnectorDelete)
	mux.HandleFunc("POST /v1/connectors/{name}/connect", s.handleConnectorStart)
	mux.HandleFunc("POST /v1/connectors/{name}/disconnect", s.handleConnectorStop)
	mux.HandleFunc("POST /v1/connectors/{name}/restart", s.handleConnectorRestart)
	mux.HandleFunc("POST /v1/connectors/{name}/test", s.handleConnectorTest)
	mux.HandleFunc("GET /v1/connectors/{name}/logs", s.handleConnectorLogs)
	mux.HandleFunc("GET /v1/connectors/{name}/cursor", s.handleConnectorCursorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}/cursor", s.handleConnectorCursorPut)
}

func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request) {
	capability := r.URL.Query().Get("capability")
	if capability != "" && capability != "actions" && capability != "events" && capability != "channel" {
		writeErr(w, http.StatusBadRequest, "validation", "capability must be actions, events, or channel")
		return
	}
	provenance := r.URL.Query().Get("provenance")
	if provenance != "" && provenance != "managed" && provenance != "inherited" && provenance != "native" {
		writeErr(w, http.StatusBadRequest, "validation", "provenance must be managed, inherited, or native")
		return
	}
	_ = s.connectors.Load() // refresh from disk; stale list is worse than a load error
	list := s.connectors.List()
	if row := s.email.ConnectorRow(); row != nil {
		list = append(list, row) // the native email channel rides the list
	}
	if row := s.hooks.ConnectorRow(); row != nil {
		list = append(list, row) // the native webhook connector rides the list
	}
	if capability != "" || provenance != "" {
		filtered := list[:0]
		for _, row := range list {
			if !rowHasCapability(row, capability) || !rowHasProvenance(row, provenance) {
				continue
			}
			filtered = append(filtered, row)
		}
		list = filtered
	}
	sort.Slice(list, func(i, j int) bool {
		return fmt.Sprint(list[i]["name"]) < fmt.Sprint(list[j]["name"])
	})
	// Name-keyed collection: limit only, no ULID cursor.
	limit := parseLimit(r)
	truncated := len(list) > limit
	if truncated {
		list = list[:limit]
	}
	resp := map[string]any{"connectors": list}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// rowHasCapability reports whether one row carries a capability. An empty
// want matches every row.
func rowHasCapability(row map[string]any, want string) bool {
	if want == "" {
		return true
	}
	caps, ok := row["capabilities"].([]string)
	if !ok {
		if anyCaps, ok := row["capabilities"].([]any); ok {
			for _, c := range anyCaps {
				if fmt.Sprint(c) == want {
					return true
				}
			}
		}
		return false
	}
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func rowHasProvenance(row map[string]any, want string) bool {
	if want == "" {
		return true
	}
	return fmt.Sprint(row["provenance"]) == want
}

func (s *Server) handleConnectorsCreate(w http.ResponseWriter, r *http.Request) {
	var d connectors.Definition
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if d.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	if err := s.connectors.Create(r.Context(), d); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(d.Name)
	if err != nil {
		writeJSON(w, http.StatusCreated, d)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleConnectorGet(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("name") {
	case "email":
		if row := s.email.ConnectorRow(); row != nil {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "email is not configured")
		return
	case "webhook":
		if row := s.hooks.ConnectorRow(); row != nil {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "webhook is not available")
		return
	}
	row, err := s.connectors.Get(r.PathValue("name"))
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorPut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var d connectors.Definition
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	d.Name = r.PathValue("name")
	if err := s.connectors.Replace(r.Context(), d); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(d.Name)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorPatch(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var p connectors.Patch
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	name := r.PathValue("name")
	if err := s.connectors.Patch(r.Context(), name, p); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(name)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	if err := s.connectors.Delete(r.Context(), r.PathValue("name")); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

// requireEventsBlock refuses lifecycle and stream routes on a connector with
// no events block. Native connectors carry events in their own service.
func (s *Server) requireEventsBlock(name string) error {
	if name == "email" || name == "webhook" {
		return nil
	}
	d, ok := s.connectors.Definition(name)
	if !ok {
		if _, err := s.connectors.Get(name); err != nil {
			return err
		}
		return fmt.Errorf("%w: connector %q is not managed", connectors.ErrInherited, name)
	}
	if d.Events == nil {
		return fmt.Errorf("%w: %q", connectors.ErrNoEvents, name)
	}
	return nil
}

func (s *Server) handleConnectorStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.connectors.Start(r.Context(), name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.connectors.Stop(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped"})
}

// handleConnectorRestart restarts a managed connector. The email row's
// restart is a forced poll and nothing else: the native channel has no
// process (plan K).
func (s *Server) handleConnectorRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "email" {
		s.handleEmailPoll(w, r)
		return
	}
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.connectors.Restart(r.Context(), name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var body struct {
		Event any `json:"event"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Event == nil {
		body.Event = map[string]any{"test": true}
	}
	_, err := s.connectors.InjectEvent(r.Context(), name, body.Event)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleConnectorLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// The native channel has no process, so it has no process log. Its
	// activity is the email log (plan K).
	if err := s.connectors.GuardReserved(name); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	path := filepath.Join(home.ConnectorsLogDir(), name+".jsonl")
	flusher, ok := w.(http.Flusher)
	if !ok {
		data, _ := os.ReadFile(path)
		writeJSON(w, http.StatusOK, map[string]any{"log": string(data)})
		return
	}
	// Headers must go out before the first log line, or an empty file + follow
	// holds the connection with no response and proxies time out as 502.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	var offset int64
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fmt.Fprintf(w, "data: %s\n\n", sc.Text())
			flusher.Flush()
		}
		offset, _ = f.Seek(0, io.SeekCurrent)
		_ = f.Close()
	}

	if r.URL.Query().Get("follow") != "1" {
		return
	}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			f2, err := os.Open(path)
			if err != nil {
				continue
			}
			st, err := f2.Stat()
			if err != nil || st.Size() <= offset {
				_ = f2.Close()
				continue
			}
			_, _ = f2.Seek(offset, io.SeekStart)
			sc2 := bufio.NewScanner(f2)
			for sc2.Scan() {
				fmt.Fprintf(w, "data: %s\n\n", sc2.Text())
				flusher.Flush()
			}
			offset, _ = f2.Seek(0, io.SeekCurrent)
			_ = f2.Close()
		}
	}
}

func (s *Server) handleConnectorCursorGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	cur, err := s.connectors.GetCursor(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var updated any
	if cur.UpdatedAt != "" {
		updated = cur.UpdatedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": cur.LastEventID, "updated_at": updated})
}

func (s *Server) handleConnectorCursorPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var body struct {
		LastEventID string `json:"last_event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body sets last_event_id to ""
	if err := s.connectors.PutCursor(r.Context(), name, body.LastEventID); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": body.LastEventID})
}

func (s *Server) writeConnectorErr(w http.ResponseWriter, err error) bool {
	var amb *connectors.AmbiguousError
	var ve *connectors.ValidationError
	switch {
	case errors.As(err, &amb):
		writeErr(w, http.StatusBadRequest, "ambiguous", err.Error())
	case errors.As(err, &ve):
		details := map[string]any{"fields": ve.Fields}
		writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), details)
	case errors.Is(err, connectors.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, connectors.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, connectors.ErrExists):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrNoEvents):
		writeErr(w, http.StatusConflict, "validation", err.Error())
	case errors.Is(err, connectors.ErrInherited):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrReserved):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		return false
	}
	return true
}
