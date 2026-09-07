package api

import (
	"bufio"
	"encoding/json"
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
	mux.HandleFunc("DELETE /v1/connectors/{name}", s.handleConnectorDelete)
	mux.HandleFunc("POST /v1/connectors/{name}/connect", s.handleConnectorStart)
	mux.HandleFunc("POST /v1/connectors/{name}/disconnect", s.handleConnectorStop)
	mux.HandleFunc("POST /v1/connectors/{name}/restart", s.handleConnectorRestart)
	// /stop is a contract alias of /disconnect (backend-0808 connectors).
	mux.HandleFunc("POST /v1/connectors/{name}/stop", s.handleConnectorStop)
	mux.HandleFunc("POST /v1/connectors/{name}/test", s.handleConnectorTest)
	mux.HandleFunc("GET /v1/connectors/{name}/logs", s.handleConnectorLogs)
	mux.HandleFunc("GET /v1/connectors/{name}/cursor", s.handleConnectorCursorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}/cursor", s.handleConnectorCursorPut)
}

func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request) {
	_ = s.connectors.Load() // refresh from disk; stale list is worse than a load error
	list := s.connectors.List()
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

func (s *Server) handleConnectorsCreate(w http.ResponseWriter, r *http.Request) {
	var d connectors.Definition
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if d.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	if err := s.connectors.Upsert(r.Context(), d); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleConnectorGet(w http.ResponseWriter, r *http.Request) {
	row, err := s.connectors.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorPut(w http.ResponseWriter, r *http.Request) {
	var d connectors.Definition
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	d.Name = r.PathValue("name")
	if err := s.connectors.Upsert(r.Context(), d); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleConnectorDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.connectors.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

func (s *Server) handleConnectorStart(w http.ResponseWriter, r *http.Request) {
	if err := s.connectors.Start(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorStop(w http.ResponseWriter, r *http.Request) {
	_ = s.connectors.Stop(r.PathValue("name"))
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped"})
}

func (s *Server) handleConnectorRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.connectors.Restart(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Event any `json:"event"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Event == nil {
		body.Event = map[string]any{"test": true}
	}
	_, err := s.connectors.InjectEvent(r.Context(), r.PathValue("name"), body.Event)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleConnectorLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path := filepath.Join(home.ConnectorsLogDir(), name+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		// empty stream
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
		return
	}
	defer f.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		// fall back to full body
		data, _ := io.ReadAll(f)
		writeJSON(w, http.StatusOK, map[string]any{"log": string(data)})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fmt.Fprintf(w, "data: %s\n\n", sc.Text())
		flusher.Flush()
	}
	// follow
	if r.URL.Query().Get("follow") == "1" {
		offset, _ := f.Seek(0, io.SeekCurrent)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				st, err := f.Stat()
				if err != nil {
					return
				}
				if st.Size() <= offset {
					continue
				}
				f2, err := os.Open(path)
				if err != nil {
					return
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
}

func (s *Server) handleConnectorCursorGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
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
	var body struct {
		LastEventID string `json:"last_event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body sets last_event_id to ""
	if err := s.connectors.PutCursor(r.Context(), name, body.LastEventID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": body.LastEventID})
}
