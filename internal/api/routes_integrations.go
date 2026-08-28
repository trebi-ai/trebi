package api

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/integrations"
	"github.com/trebi-ai/trebi/internal/tmux"
)

//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeIntegrations(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/integrations", s.handleIntegrations)
	mux.HandleFunc("POST /v1/integrations", s.handleIntegrationsCreate)
	mux.HandleFunc("GET /v1/integrations/{name}", s.handleIntegrationGet)
	mux.HandleFunc("PUT /v1/integrations/{name}", s.handleIntegrationPut)
	mux.HandleFunc("DELETE /v1/integrations/{name}", s.handleIntegrationDelete)
	mux.HandleFunc("POST /v1/integrations/{name}/connect", s.handleIntegrationStart)
	mux.HandleFunc("POST /v1/integrations/{name}/disconnect", s.handleIntegrationStop)
	mux.HandleFunc("POST /v1/integrations/{name}/restart", s.handleIntegrationRestart)
	// /stop is a contract alias of /disconnect (backend-0808 integrations).
	mux.HandleFunc("POST /v1/integrations/{name}/stop", s.handleIntegrationStop)
	mux.HandleFunc("POST /v1/integrations/{name}/test", s.handleIntegrationTest)
	mux.HandleFunc("GET /v1/integrations/{name}/logs", s.handleIntegrationLogs)
	mux.HandleFunc("GET /v1/integrations/{name}/cursor", s.handleIntegrationCursorGet)
	mux.HandleFunc("PUT /v1/integrations/{name}/cursor", s.handleIntegrationCursorPut)
	mux.HandleFunc("GET /v1/harnesses", s.handleHarnesses)
	mux.HandleFunc("POST /v1/harnesses/{name}/ensure", s.handleHarnessEnsure)
	mux.HandleFunc("GET /v1/harnesses/{name}/auth", s.handleHarnessAuthGet)
	mux.HandleFunc("POST /v1/harnesses/{name}/auth", s.handleHarnessAuthPost)
}

func (s *Server) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	_ = s.integ.Load() // refresh from disk; stale list is worse than a load error
	list := s.integ.List()
	sort.Slice(list, func(i, j int) bool {
		return fmt.Sprint(list[i]["name"]) < fmt.Sprint(list[j]["name"])
	})
	// Name-keyed collection: limit only, no ULID cursor.
	limit := parseLimit(r)
	if len(list) > limit {
		list = list[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": list})
}

func (s *Server) handleIntegrationsCreate(w http.ResponseWriter, r *http.Request) {
	var d integrations.Definition
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if d.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	if err := s.integ.Upsert(r.Context(), d); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleIntegrationGet(w http.ResponseWriter, r *http.Request) {
	row, err := s.integ.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleIntegrationPut(w http.ResponseWriter, r *http.Request) {
	var d integrations.Definition
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	d.Name = r.PathValue("name")
	if err := s.integ.Upsert(r.Context(), d); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleIntegrationDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.integ.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

func (s *Server) handleIntegrationStart(w http.ResponseWriter, r *http.Request) {
	if err := s.integ.Start(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleIntegrationStop(w http.ResponseWriter, r *http.Request) {
	_ = s.integ.Stop(r.PathValue("name"))
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped"})
}

func (s *Server) handleIntegrationRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.integ.Restart(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleIntegrationTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Event any `json:"event"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Event == nil {
		body.Event = map[string]any{"test": true}
	}
	_, err := s.integ.InjectEvent(r.Context(), r.PathValue("name"), body.Event)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleIntegrationLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path := filepath.Join(home.IntegrationsLogDir(), name+".jsonl")
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

func (s *Server) handleIntegrationCursorGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var lastID, updated string
	err := s.st.DB().QueryRowContext(r.Context(),
		`SELECT last_event_id, updated_at FROM integration_cursors WHERE name=?`, name).
		Scan(&lastID, &updated)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": "", "updated_at": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": lastID, "updated_at": updated})
}

func (s *Server) handleIntegrationCursorPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		LastEventID string `json:"last_event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body sets last_event_id to ""
	now := time.Now().UTC().Format(time.RFC3339)
	// integration_cursors is internal bookkeeping, not a B10 bus entity.
	_, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO integration_cursors (name, last_event_id, updated_at)
		VALUES (?,?,?) ON CONFLICT(name) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
		name, body.LastEventID, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": body.LastEventID})
}

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"harnesses": harness.List()})
}

func (s *Server) handleHarnessEnsure(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dir := home.HarnessDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Materialize lifecycle if tool known
	tool, ok := tmux.ByName(name)
	if ok {
		if life := tool.Lifecycle(); life != nil {
			_ = life.Install(config.BaseDir(), config.BinDir())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "home": dir, "ensured": true})
}

func (s *Server) handleHarnessAuthGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var authOK sql.NullInt64
	_ = s.st.DB().QueryRowContext(r.Context(), `SELECT auth_ok FROM harness_state WHERE name=?`, name).Scan(&authOK)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "auth_ok": authOK.Valid && authOK.Int64 == 1, "mode": "shared",
	})
}

func (s *Server) handleHarnessAuthPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	now := time.Now().UTC().Format(time.RFC3339)
	// harness_state is internal bookkeeping, not a B10 bus entity.
	_, _ = s.st.DB().ExecContext(r.Context(), `INSERT INTO harness_state (name, auth_ok, updated_at)
		VALUES (?,1,?) ON CONFLICT(name) DO UPDATE SET auth_ok=1, updated_at=excluded.updated_at`, name, now) // probe row; auth_ok is best-effort
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "auth_ok": true})
}
