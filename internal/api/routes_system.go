package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	// streamPollInterval is the bus poll cadence for /v1/stream.
	streamPollInterval = 200 * time.Millisecond
	// streamKeepalive is the max idle time before an SSE comment flush.
	streamKeepalive = 15 * time.Second
)

func (s *Server) routeSystem(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/system/health", s.handleHealth)
	mux.HandleFunc("POST /v1/system/drain", s.handleDrain)
	mux.HandleFunc("POST /v1/system/undrain", s.handleUndrain)
	mux.HandleFunc("POST /v1/system/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /v1/system/doctor", s.handleDoctor)
	mux.HandleFunc("POST /v1/system/doctor/{finding_id}/fix", s.handleDoctorFix)
	mux.HandleFunc("GET /v1/system/logs", s.handleSystemLogs)
	mux.HandleFunc("GET /v1/system/update", s.handleSystemUpdate)
	mux.HandleFunc("GET /v1/cloud/status", s.handleCloudStatus)
	mux.HandleFunc("POST /v1/cloud/link", s.handleCloudLink)
	mux.HandleFunc("DELETE /v1/cloud/link", s.handleCloudUnlink)
	mux.HandleFunc("GET /v1/stream", s.handleStream)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	drain := s.drain
	s.mu.RUnlock()
	schema, _ := s.st.SchemaVersion()
	running, _ := s.runs.RunningCount(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        Version,
		"api":            "v1",
		"uptime_ms":      time.Since(s.startedAt).Milliseconds(),
		"schema_version": schema,
		"drain_state":    map[string]any{"draining": drain, "running_count": running},
		"waiting_count":  s.runs.WaitingCount(),
		"shadow":         s.shadow,
	})
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.drain = true
	s.mu.Unlock()
	if err := s.st.SetFlag("drain", "1"); err != nil {
		s.log.Warn("set flag", "flag", "drain", "error", err)
	}
	if _, err := s.st.Append(r.Context(), store.BusEvent{Topic: "system.drain.entered"}); err != nil {
		s.log.Warn("bus append", "topic", "system.drain.entered", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"draining": true})
}

func (s *Server) handleUndrain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.drain = false
	s.mu.Unlock()
	if err := s.st.DeleteFlag("drain"); err != nil {
		s.log.Warn("delete flag", "flag", "drain", "error", err)
	}
	if _, err := s.st.Append(r.Context(), store.BusEvent{Topic: "system.drain.cancelled"}); err != nil {
		s.log.Warn("bus append", "topic", "system.drain.cancelled", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"draining": false})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting_down"})
	// Delay shutdown so the response can reach the client; Shutdown ends the goroutine.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = s.Shutdown(context.Background()) // response is already sent; shutdown failure has no client to notify
	}()
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	info := home.DoctorDaemon()
	findings := s.doctorFindings()
	if info.APIJSONURL != "" && !info.APIJSONReachable {
		findings = append(findings, map[string]any{
			"id": "api-json-stale", "severity": "warn", "code": "api_json_stale",
			"message": fmt.Sprintf("api.json URL %s does not answer /v1/system/health", info.APIJSONURL),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings":           findings,
		"daemon_lock_held":   info.LockHeld,
		"daemon_lock_pid":    info.LockPID,
		"api_json_url":       emptyNil(info.APIJSONURL),
		"api_json_reachable": info.APIJSONReachable,
	})
}

func (s *Server) handleCloudStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"linked": false, "status": "not_linked"})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // send headers before the first event

	cursor, topics := s.streamOpts(w, r, flusher)

	// Catch-up, then poll until the client disconnects or the server stops.
	lastWrite := time.Now()
	for {
		events, err := s.st.ListBus(r.Context(), cursor, topics, 100)
		if err != nil {
			if r.Context().Err() == nil {
				s.log.Warn("stream.list_bus", "error", err)
			}
			return
		}
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(ev))
			cursor = ev.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			lastWrite = time.Now()
		} else if time.Since(lastWrite) >= streamKeepalive {
			// SSE comment line keeps idle proxy connections alive.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
			lastWrite = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-time.After(streamPollInterval):
		}
	}
}

// streamOpts parses cursor/topics and emits a synthetic reset when the
// cursor fell behind the retained bus window.
func (s *Server) streamOpts(w http.ResponseWriter, r *http.Request, flusher http.Flusher) (int64, []string) {
	cursor := int64(0)
	if c := r.URL.Query().Get("cursor"); c != "" {
		cursor, _ = strconv.ParseInt(c, 10, 64)
	}
	var topics []string
	if t := r.URL.Query().Get("topics"); t != "" {
		topics = strings.Split(t, ",")
	}
	oldest, _ := s.st.OldestSeq(r.Context())
	if cursor > 0 && oldest > 0 && cursor < oldest {
		// synthetic reset
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"topic": store.TopicSystemReset, "seq": oldest}))
		flusher.Flush()
		cursor = oldest - 1
	}
	return cursor, topics
}

func (s *Server) handleAgentState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.runs.AgentSnapshot()})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var total, running int
	_ = s.st.DB().QueryRowContext(r.Context(), `SELECT COUNT(*) FROM runs`).Scan(&total)
	_ = s.st.DB().QueryRowContext(r.Context(), `SELECT COUNT(*) FROM runs WHERE status='running'`).Scan(&running)
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "running": running})
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.Parse(time.RFC3339Nano, raw)
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed since: want RFC3339")
			return
		}
		since = t
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.alerts.ListAlerts(r.Context(), since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if events == nil {
		events = []store.StreamEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": events})
}
