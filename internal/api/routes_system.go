package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/guides"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	// streamPollInterval is the bus poll cadence for /v1/stream.
	streamPollInterval = 200 * time.Millisecond
	// streamKeepalive is the max idle time before an SSE comment flush.
	streamKeepalive = 15 * time.Second
	// systemLogCap is the default and max number of lines GET /v1/system/logs returns.
	systemLogCap = 200
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
	mux.HandleFunc("GET /v1/system/guides", s.handleGuidesList)
	mux.HandleFunc("GET /v1/system/guides/{topic}", s.handleGuideGet)
	mux.HandleFunc("GET /v1/system/overview", s.handleOverview)
	mux.HandleFunc("GET /v1/cloud/status", s.handleCloudStatus)
	mux.HandleFunc("POST /v1/cloud/link", s.handleCloudLink)
	mux.HandleFunc("DELETE /v1/cloud/link", s.handleCloudUnlink)
	mux.HandleFunc("GET /v1/stream", s.handleStream)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	schema, _ := s.st.SchemaVersion()
	running, _ := s.runs.RunningCount(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        Version,
		"api":            "v1",
		"uptime_ms":      time.Since(s.startedAt).Milliseconds(),
		"schema_version": schema,
		"drain_state":    map[string]any{"draining": s.isDraining(), "running_count": running},
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

// handleOverview answers "is trebi ready, and what needs me?" in one call. It
// reads what is cheap and reports zero rather than failing on a broken part.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	running, _ := s.runs.RunningCount(ctx)
	schema, _ := s.st.SchemaVersion()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        Version,
		"schema_version": schema,
		"uptime_ms":      time.Since(s.startedAt).Milliseconds(),
		"drain_state":    map[string]any{"draining": s.isDraining(), "running_count": running},
		"shadow":         s.shadow,
		"doctor":         s.doctorSummary(),
		"harnesses":      s.harnessAuthTable(ctx),
		"workspaces":     s.workspaceCount(ctx),
		"runs":           map[string]any{"running": running, "waiting": s.runs.WaitingCount()},
		"inbox":          s.inboxCounts(ctx),
		"next_due_at":    s.nextDueAt(ctx),
	})
}

// doctorSummary counts findings by severity.
func (s *Server) doctorSummary() map[string]int {
	out := map[string]int{"ok": 0, "warn": 0, "error": 0}
	for _, f := range s.doctorFindings() {
		if sev, _ := f["severity"].(string); sev != "" {
			out[sev]++
		}
	}
	return out
}

// harnessAuthTable maps each known harness to whether its auth is ok.
func (s *Server) harnessAuthTable(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if s.harness == nil {
		return out
	}
	states, err := s.harness.States(ctx)
	if err != nil {
		return out
	}
	for name, st := range states {
		out[name] = st.AuthOK.Valid && st.AuthOK.Int64 == 1
	}
	return out
}

func (s *Server) workspaceCount(ctx context.Context) int {
	list, err := s.ws.List(ctx)
	if err != nil {
		return 0
	}
	return len(list)
}

// inboxCounts returns the unread total and the unread approvals inside it.
func (s *Server) inboxCounts(ctx context.Context) map[string]int {
	out := map[string]int{"unread": 0, "approvals": 0}
	if s.alerts == nil {
		return out
	}
	items, err := s.alerts.List(ctx, alerts.ListOpts{Status: string(alerts.InboxUnread), Limit: maxListLimit})
	if err != nil {
		return out
	}
	out["unread"] = len(items)
	for _, it := range items {
		if it.Kind == alerts.InboxApproval {
			out["approvals"]++
		}
	}
	return out
}

// nextDueAt returns the earliest scheduled fire time, or "" when none is due.
func (s *Server) nextDueAt(ctx context.Context) string {
	if s.sched == nil {
		return ""
	}
	rows, err := s.sched.List(ctx, schedule.ListOpts{Limit: maxListLimit})
	if err != nil {
		return ""
	}
	next := ""
	for _, row := range rows {
		if row.NextDueAt != "" && (next == "" || row.NextDueAt < next) {
			next = row.NextDueAt
		}
	}
	return next
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
	follow := r.URL.Query().Get("follow") != "0"

	// Catch-up, then poll until the client disconnects or the server stops.
	// follow=0 (CLI --limit) dumps history and closes; --limit is a max.
	lastWrite := time.Now()
	lag := time.NewTicker(streamPollInterval)
	defer lag.Stop()
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
		if !follow && len(events) < 100 {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-s.st.BusWake():
		case <-lag.C:
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

func (s *Server) doctorFindings() []map[string]any {
	src := alerts.CollectDoctorFindings()
	src = append(src, s.nativeAssistantDoctor()...)
	src = append(src, s.apiBindDoctor()...)
	src = append(src, s.agentLayoutDoctor()...)
	src = append(src, s.memoryDoctor()...)
	out := make([]map[string]any, 0, len(src))
	for _, f := range src {
		out = append(out, map[string]any{
			"id": f.ID, "severity": f.Severity, "code": f.Code, "message": f.Message,
		})
	}
	return out
}

// agentLayoutDoctor reports agent layout problems from the job index (Part K).
// No automatic fix exists; --fix answers "no automatic fix" for these ids.
func (s *Server) agentLayoutDoctor() []alerts.DoctorFinding {
	rows, err := s.ws.ListJobs(context.Background(), "")
	if err != nil {
		return nil
	}
	lr := make([]job.LayoutRow, len(rows))
	for i, r := range rows {
		lr[i] = job.LayoutRow{WorkspaceID: r.WorkspaceID, Name: r.Name, Path: r.Path, Kind: r.Kind, Owner: r.Owner}
	}
	var out []alerts.DoctorFinding
	for _, iss := range job.LayoutIssues(lr) {
		out = append(out, alerts.DoctorFinding{
			ID: iss.Code + ":" + iss.Path, Severity: iss.Severity(), Code: iss.Code, Message: iss.Message(),
		})
	}
	return out
}

func (s *Server) apiBindDoctor() []alerts.DoctorFinding {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return nil
	}
	if !cfg.API.TLS.Insecure || cfg.API.Loopback() || cfg.API.TLSEnabled() {
		return nil
	}
	if !cfg.API.PublicBind() {
		return nil
	}
	return []alerts.DoctorFinding{{
		ID: "api-tls-insecure", Severity: "error", Code: "api_tls_insecure",
		Message: fmt.Sprintf("api.tls.insecure is set on a public bind (%s); use cert/key or a private/overlay address", cfg.API.HostOrDefault()),
	}}
}

// memoryDoctor warns on pre-v1 MEMORY.md files and on memory_invalid alerts
// from the last 7 days (B22 §3.3 rev 2). No automatic fix; migrate with
// `trebi memory validate --fix` or the owner-scoped `memory migrate`.
func (s *Server) memoryDoctor() []alerts.DoctorFinding {
	var out []alerts.DoctorFinding
	list, err := s.ws.List(context.Background())
	if err != nil {
		return nil
	}
	dirs := map[string]string{} // owner folder -> workspace path, for rel names
	for _, ws := range list {
		dirs[ws.Path] = ws.Path
		rows, err := s.ws.ListJobs(context.Background(), ws.ID)
		if err != nil {
			continue
		}
		for _, row := range rows {
			dirs[filepath.Dir(row.Path)] = ws.Path
		}
	}
	for dir, wsPath := range dirs {
		abs := filepath.Join(dir, "MEMORY.md")
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		doc, _ := memory.Parse(data)
		if !doc.Legacy {
			continue
		}
		rel, err := workspaceRel(wsPath, abs)
		if err != nil {
			rel = abs
		}
		out = append(out, alerts.DoctorFinding{
			ID: "memory-legacy:" + rel, Severity: "warn", Code: "memory_legacy",
			Message: fmt.Sprintf("%s uses the pre-v1 ledger; run: trebi memory validate --fix %s", rel, abs),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return append(out, s.memoryAlertDoctor()...)
}

// memoryAlertDoctor counts memory_invalid alerts raised in the last 7 days.
func (s *Server) memoryAlertDoctor() []alerts.DoctorFinding {
	events, err := s.alerts.ListAlerts(context.Background(), time.Now().Add(-7*24*time.Hour), 200)
	if err != nil {
		return nil
	}
	n := 0
	for _, ev := range events {
		var attrs struct {
			Kind string `json:"kind"`
		}
		if ev.Topic != "alert.raised" || json.Unmarshal(ev.Attrs, &attrs) != nil {
			continue
		}
		if attrs.Kind == string(alerts.KindMemoryInvalid) {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return []alerts.DoctorFinding{{
		ID: "memory-invalid-alerts", Severity: "warn", Code: "memory_invalid_recent",
		Message: fmt.Sprintf("%d memory_invalid alerts in the last 7 days; check the memory tab of the named jobs", n),
	}}
}

func (s *Server) nativeAssistantDoctor() []alerts.DoctorFinding {
	cfg, err := config.Load()
	if err != nil || cfg == nil || !cfg.Assistant.Enabled() {
		return nil
	}
	list, err := s.ws.List(context.Background())
	if err != nil {
		return nil
	}
	var out []alerts.DoctorFinding
	for _, ws := range list {
		j, err := job.ParseFile(filepath.Join(ws.Path, "TREBI.md"))
		if err != nil || j == nil || strings.TrimSpace(j.Harness) == "" {
			continue
		}
		out = append(out, alerts.DoctorFinding{
			ID: "assistant-harness-" + ws.ID, Severity: "info", Code: "assistant_harness_ignored",
			Message: "TREBI.md harness: no longer selects the assistant runtime (it still pins job runs). Native is on via assistant.model; escape hatch is runtime: tui.",
		})
		break
	}
	return out
}

func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	path := home.DaemonLogPath()
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	q := r.URL.Query()
	n := systemLogCap
	if raw := q.Get("lines"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			n = v
		}
	}
	lines := filterRunLines(strings.Split(string(data), "\n"), q.Get("run"))
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": filterLogLines(lines, n, q.Get("level"), q.Get("grep")),
	})
}

// filterRunLines keeps the daemon records that belong to one run. Access log
// lines name the run in the route they served but say nothing about the run
// itself, so they are dropped. An empty run keeps every line.
func filterRunLines(lines []string, run string) []string {
	if run == "" {
		return lines
	}
	out := make([]string, 0, 16)
	for _, line := range lines {
		if !strings.Contains(line, run) {
			continue
		}
		if strings.Contains(line, "msg=api.request") || strings.Contains(line, `msg="api.request"`) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// filterLogLines keeps lines matching level and grep substrings (case-insensitive),
// then the last n (default and max systemLogCap).
func filterLogLines(lines []string, n int, level, grep string) []string {
	if n <= 0 || n > systemLogCap {
		n = systemLogCap
	}
	level = strings.ToLower(level)
	grep = strings.ToLower(grep)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		if level != "" && !strings.Contains(low, level) {
			continue
		}
		if grep != "" && !strings.Contains(low, grep) {
			continue
		}
		out = append(out, line)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// handleSystemUpdate is a stub: it does not consult a release feed.
// source is empty until a real feed exists so the UI can show versions without a verdict.
func (s *Server) handleSystemUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"current": Version, "latest": Version, "update_available": false, "source": "",
	})
}

func (s *Server) handleGuidesList(w http.ResponseWriter, r *http.Request) {
	list := guides.List()
	items := make([]map[string]string, 0, len(list))
	for _, g := range list {
		items = append(items, map[string]string{
			"topic": g.Topic, "title": g.Title, "description": g.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGuideGet(w http.ResponseWriter, r *http.Request) {
	g, err := guides.Get(r.PathValue("topic"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "guide not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"topic": g.Topic, "title": g.Title, "description": g.Description, "body": g.Body,
	})
}

func (s *Server) handleCloudLink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"linked": false, "status": "not_configured", "note": "cloudlink broker not configured"})
}

func (s *Server) handleCloudUnlink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"linked": false})
}
