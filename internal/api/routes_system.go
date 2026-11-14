package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/guides"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/ui"
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
	mux.HandleFunc("POST /v1/cloud/link-request", s.handleCloudLinkRequest)
	mux.HandleFunc("POST /v1/cloud/link-request/resend", s.handleCloudLinkResend)
	mux.HandleFunc("DELETE /v1/cloud/link-request", s.handleCloudLinkCancel)
	mux.HandleFunc("GET /v1/cloud/addresses", s.handleCloudAddresses)
	mux.HandleFunc("POST /v1/system/notifications/request", s.handleNotificationsRequest)
	mux.HandleFunc("GET /v1/system/notifications", s.handleNotificationsGet)
	mux.HandleFunc("PUT /v1/system/notifications", s.handleNotificationsPut)
	mux.HandleFunc("GET /v1/stream", s.handleStream)
}

// The values of the notification status that the tray reports.
const (
	notificationsNotDetermined = "not_determined"
	notificationsDenied        = "denied"
	notificationsAllowed       = "allowed"
	notificationsUnknown       = "unknown"
)

// handleNotificationsRequest asks the tray to show the macOS permission
// prompt. The tray follows the bus topic.
func (s *Server) handleNotificationsRequest(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Append(r.Context(), store.BusEvent{Topic: "system.notifications_requested"}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleNotificationsGet answers the last status that the tray reported.
// tray is false until a report arrives since the daemon started.
func (s *Server) handleNotificationsGet(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	status := s.notifications
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": cmp.Or(status, notificationsUnknown),
		"tray":   status != "",
	})
}

// handleNotificationsPut stores the status that the tray reports.
func (s *Server) handleNotificationsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	switch body.Status {
	case notificationsNotDetermined, notificationsDenied, notificationsAllowed:
	default:
		writeErr(w, http.StatusBadRequest, "validation", "status must be not_determined, denied, or allowed")
		return
	}
	s.mu.Lock()
	s.notifications = body.Status
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	schema, _ := s.st.SchemaVersion()
	running, _ := s.runs.RunningCount(r.Context())
	body := map[string]any{
		"status":         "ok",
		"version":        Version,
		"edition":        s.edition(),
		"api":            "v1",
		"uptime_ms":      time.Since(s.startedAt).Milliseconds(),
		"schema_version": schema,
		"drain_state":    map[string]any{"draining": s.isDraining(), "running_count": running},
		"waiting_count":  s.runs.WaitingCount(),
		"shadow":         s.shadow,
	}
	if s.ext.EditionVersion != "" {
		body["edition_version"] = s.ext.EditionVersion
	}
	if uv := s.uiVersion(); uv != "" {
		body["ui_version"] = uv
	}
	writeJSON(w, http.StatusOK, body)
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
	body := map[string]any{
		"version":        Version,
		"schema_version": schema,
		"uptime_ms":      time.Since(s.startedAt).Milliseconds(),
		"drain_state":    map[string]any{"draining": s.isDraining(), "running_count": running},
		"shadow":         s.shadow,
		"doctor":         s.cachedDoctorSummary(),
		"harnesses":      s.harnessAuthTable(ctx),
		"workspaces":     s.workspaceCount(ctx),
		"runs":           map[string]any{"running": running, "waiting": s.runs.WaitingCount()},
		"inbox":          s.inboxCounts(ctx, s.workspaceID(ctx, r.URL.Query().Get("workspace"))),
		"next_due_at":    s.nextDueAt(ctx),
	}
	if uv := s.uiVersion(); uv != "" {
		body["ui_version"] = uv
	}
	writeJSON(w, http.StatusOK, body)
}

// startDoctorRefresher computes the doctor summary once, then refreshes it
// every minute and on every config change. The request path never spawns a
// probe process (plan 2026-09-26 H).
func (s *Server) startDoctorRefresher() {
	s.doctorMu.Lock()
	if s.doctorKick == nil {
		s.doctorKick = make(chan struct{}, 1)
	}
	s.doctorMu.Unlock()
	s.refreshDoctor()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.refreshDoctor()
			case <-s.doctorKick:
				s.refreshDoctor()
			}
		}
	}()
}

// refreshDoctor runs one doctor round into the cache.
func (s *Server) refreshDoctor() {
	summary := s.doctorSummary()
	s.doctorMu.Lock()
	s.doctorCached = summary
	s.doctorMu.Unlock()
}

// kickDoctor asks the refresher for one fresh round. It never blocks.
func (s *Server) kickDoctor() {
	s.doctorMu.Lock()
	kick := s.doctorKick
	s.doctorMu.Unlock()
	if kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// cachedDoctorSummary serves the background value. Before the refresher runs
// (a handler test, or a request before serve), it computes once, exactly as
// the old request-path behavior did.
func (s *Server) cachedDoctorSummary() map[string]int {
	s.doctorMu.Lock()
	cached := s.doctorCached
	s.doctorMu.Unlock()
	if cached == nil {
		s.refreshDoctor()
		s.doctorMu.Lock()
		cached = s.doctorCached
		s.doctorMu.Unlock()
	}
	out := make(map[string]int, len(cached))
	for k, v := range cached {
		out[k] = v
	}
	return out
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

// inboxCounts returns the open unread total and the open unread approvals
// inside it. A set workspace keeps its rows plus global rows, as the list does.
func (s *Server) inboxCounts(ctx context.Context, workspace string) map[string]int {
	out := map[string]int{"unread": 0, "approvals": 0}
	if s.alerts == nil {
		return out
	}
	c, err := s.alerts.Counts(ctx, workspace)
	if err != nil {
		return out
	}
	for _, n := range c.Unread {
		out["unread"] += n
	}
	out["approvals"] = c.Unread[alerts.InboxApproval]
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

// handleCloudStatus reports the cached link state (plan 2026-09-22 Part
// G). It never 404s: an unlinked daemon answers linked:false, which is
// the answer rather than an error.
func (s *Server) handleCloudStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cloud.Status())
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
	// WebKit holds a fetch until the first body byte, so a quiet bus would
	// look offline until the keepalive.
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

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
// cursor fell behind the retained bus window. cursor=latest skips the
// retained history: a client that loads state over REST needs only new frames.
func (s *Server) streamOpts(w http.ResponseWriter, r *http.Request, flusher http.Flusher) (int64, []string) {
	cursor := int64(0)
	switch c := r.URL.Query().Get("cursor"); c {
	case "":
	case "latest":
		cursor, _ = s.st.LatestSeq(r.Context())
	default:
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

// handleDoctorFix runs the automatic fix of one finding, found by its exact
// id. A finding with no fix answers 409 and its inbox item stays open.
func (s *Server) handleDoctorFix(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("finding_id")
	var finding alerts.DoctorFinding
	for _, f := range s.alerts.DoctorFindings() {
		if f.ID == id {
			finding = f
			break
		}
	}
	var action string
	switch finding.Fix {
	case alerts.FixWriteConfig:
		if _, err := config.Seed(); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		action = "wrote_default_config"
	case alerts.FixHarnessHome:
		if !harness.Known(finding.Harness) {
			writeErr(w, http.StatusConflict, "conflict", "This finding has no automatic fix")
			return
		}
		if err := os.MkdirAll(home.HarnessDir(finding.Harness), 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		action = "ensured_harness_home"
	default:
		writeErr(w, http.StatusConflict, "conflict", "This finding has no automatic fix")
		return
	}
	s.log.Info("doctor.fix_applied", "id", id, "fix", finding.Fix)
	_ = s.alerts.ResolveSubject(r.Context(), "doctor/"+id, "auto")
	writeJSON(w, http.StatusOK, map[string]any{"fixed": id, "action": action})
}

func (s *Server) doctorFindings() []map[string]any {
	src := s.alerts.DoctorFindings()
	src = append(src, s.assistantDoctor()...)
	src = append(src, s.apiBindDoctor()...)
	src = append(src, s.agentLayoutDoctor()...)
	src = append(src, s.memoryDoctor()...)
	src = append(src, s.emailDoctor()...)
	src = append(src, s.connectorDoctor()...)
	src = append(src, s.cloudDoctor()...)
	src = append(src, s.harnessRuntimeDoctor(context.Background())...)
	src = append(src, s.chains.Doctor(context.Background())...)
	out := make([]map[string]any, 0, len(src))
	for _, f := range src {
		m := map[string]any{"id": f.ID, "severity": f.Severity, "code": f.Code, "message": f.Message}
		if f.Fix != "" {
			m["fix"] = f.Fix
		}
		out = append(out, m)
	}
	return out
}

// emailDoctor reports the email channel findings (plan 2026-09-19 Part K):
// a misconfigured block, an unresolvable token, a permanently failed alert
// send, a Worker unreachable for more than three polls, and a spool older
// than one hour.
func (s *Server) emailDoctor() []alerts.DoctorFinding {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return nil
	}
	var out []alerts.DoctorFinding
	var ce *config.ConfigError
	if err := cfg.Validate(); errors.As(err, &ce) {
		for _, fe := range ce.Errors {
			if fe.Field != "email" && !strings.HasPrefix(fe.Field, "email.") {
				continue
			}
			out = append(out, alerts.DoctorFinding{
				ID: "email-config:" + fe.Field, Severity: "error", Code: "email_config",
				Message: fe.Field + ": " + fe.Msg,
			})
		}
	}
	if !cfg.Email.Enabled() {
		return out
	}
	if _, err := cfg.Email.ResolveToken(); err != nil {
		out = append(out, alerts.DoctorFinding{
			ID: "email-token", Severity: "error", Code: "email_token",
			Message: err.Error() + " (trebi secret set <name>)",
		})
	}
	st := s.email.Status(context.Background())
	if st.SendError != "" {
		out = append(out, alerts.DoctorFinding{
			ID: "email-send-failed", Severity: "error", Code: "email_send_failed",
			Message: "last alert send failed: " + st.SendError,
		})
	}
	if st.Failures > 3 {
		out = append(out, alerts.DoctorFinding{
			ID: "email-worker-unreachable", Severity: "error", Code: "email_worker_unreachable",
			Message: fmt.Sprintf("Worker unreachable for %d polls: %s", st.Failures, st.LastError),
		})
	}
	if st.WorkerHealth != nil && st.WorkerHealth.SpoolOldestAgeMS > int64(time.Hour/time.Millisecond) {
		out = append(out, alerts.DoctorFinding{
			ID: "email-spool-stalled", Severity: "warn", Code: "email_spool_stalled",
			Message: fmt.Sprintf("spool holds mail %s old; trebid is not pulling",
				time.Duration(st.WorkerHealth.SpoolOldestAgeMS)*time.Millisecond),
		})
	}
	return out
}

// connectorDoctor reports the connector findings (plan 2026-09-28 Part E.12).
func (s *Server) connectorDoctor() []alerts.DoctorFinding {
	if s.connectors == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out []alerts.DoctorFinding
	for _, f := range s.connectors.Doctor(ctx) {
		out = append(out, alerts.DoctorFinding{
			ID: fmt.Sprintf("%s:%s:%08x", f.Code, f.Connector, crc32.ChecksumIEEE([]byte(f.Message))), Severity: f.Severity, Code: f.Code, Message: f.Message,
		})
	}
	for reg, err := range s.catalogRegistry().CatalogProblems() {
		out = append(out, alerts.DoctorFinding{
			ID: "catalog_signature_invalid:" + reg, Severity: "error", Code: "catalog_signature_invalid",
			Message: fmt.Sprintf("registry %s: %v; search and install use the last good index", reg, err),
		})
	}
	return out
}

// cloudDoctor reports the hosted link findings (plan 2026-09-22 Part D):
// cloud block config errors, and both mail transports configured at once,
// which leaves the Worker path running and needs the operator's eye.
func (s *Server) cloudDoctor() []alerts.DoctorFinding {
	var out []alerts.DoctorFinding
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return nil
	}
	var ce *config.ConfigError
	if err := cfg.Validate(); errors.As(err, &ce) {
		for _, fe := range ce.Errors {
			if fe.Field != "cloud" && !strings.HasPrefix(fe.Field, "cloud.") {
				continue
			}
			out = append(out, alerts.DoctorFinding{
				ID: "cloud-config:" + fe.Field, Severity: "error", Code: "cloud_config",
				Message: fe.Field + ": " + fe.Msg,
			})
		}
	}
	if s.cloud == nil {
		return out
	}
	ent := s.cloud.Entitlement()
	bearer := cfg.Email.Enabled()
	hosted := cfg.Cloud.Active() && ent.Linked && ent.Has(entitle.HostedIngress)
	if bearer && hosted {
		out = append(out, alerts.DoctorFinding{
			ID: "cloud-email-conflict", Severity: "warn", Code: "email_cloud_conflict",
			Message: "email.worker and a hosted cloud link are both set; the Worker transport wins. Remove one.",
		})
	}
	if ent.Linked && s.cloud.Status().Fallback {
		out = append(out, alerts.DoctorFinding{
			ID: "cloud-fallback-active", Severity: "info", Code: "cloud_fallback_active",
			Message: "This network blocks the main Trebi Cloud address. Trebi uses the backup address.",
		})
	}
	// Only an explicit opt-in warns; the default is on for every machine.
	if cfg.Cloud.RemoteMCP != nil && *cfg.Cloud.RemoteMCP {
		switch {
		case !ent.Linked:
			out = append(out, alerts.DoctorFinding{
				ID: "cloud-remote-mcp-unlinked", Severity: "warn", Code: "remote_mcp_unlinked",
				Message: "Remote access for Claude and ChatGPT is on, but this machine is not linked to Trebi Cloud.",
			})
		case !ent.Has(entitle.RemoteAccess):
			out = append(out, alerts.DoctorFinding{
				ID: "cloud-remote-mcp-plan", Severity: "info", Code: "remote_mcp_plan",
				Message: "Remote access for Claude and ChatGPT is on, but your plan does not include it. Upgrade to Pro Cloud.",
			})
		}
	}
	return append(out, s.mobileDoctor(cfg, ent)...)
}

// mobileDoctor warns when paired phones cannot work (plan 2026-10-09 B10).
func (s *Server) mobileDoctor(cfg *config.Config, ent entitle.State) []alerts.DoctorFinding {
	if !cfg.Cloud.MobileOn() || !ent.Linked {
		return nil
	}
	var out []alerts.DoctorFinding
	if !ent.Has(entitle.RemoteAccess) {
		out = append(out, alerts.DoctorFinding{
			ID: "cloud-mobile-plan", Severity: "info", Code: "mobile_plan",
			Message: "Phones and tablets are on, but your plan does not include remote access. Upgrade to Pro Cloud.",
		})
	}
	if _, ok := ui.FS(); !ok && s.ext.UI == nil {
		out = append(out, alerts.DoctorFinding{
			ID: "cloud-mobile-no-ui", Severity: "warn", Code: "mobile_no_ui",
			Message: "This build has no embedded UI, so a paired phone gets no page. Install a release build.",
		})
	}
	if s.mobile != nil && cfg.Mobile.Direct.On() {
		if err := s.mobile.DirectError(); err != nil {
			out = append(out, alerts.DoctorFinding{
				ID: "mobile-direct-port", Severity: "warn", Code: "mobile_direct_bind",
				Message: fmt.Sprintf("Phones and tablets on the same network cannot connect directly, because port %d is not available. Phones still connect through Trebi Cloud. Change the port in Settings, then restart Trebi.", cfg.Mobile.Direct.PortOrDefault()),
			})
		}
	}
	return out
}

// harnessRuntimeDoctor maps the subprocess-runtime posture findings (Part J).
func (s *Server) harnessRuntimeDoctor(ctx context.Context) []alerts.DoctorFinding {
	findings := s.runs.HarnessFindings(ctx)
	out := make([]alerts.DoctorFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, alerts.DoctorFinding{
			ID: f.ID, Severity: f.Severity, Code: f.Code, Message: f.Message,
		})
	}
	return out
}

// agentLayoutDoctor reports agent layout problems from the job index (Part K),
// and each job outside an agent. No automatic fix exists; --fix answers "no
// automatic fix" for these ids. The fix of a job outside an agent is a move.
func (s *Server) agentLayoutDoctor() []alerts.DoctorFinding {
	ctx := context.Background()
	rows, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return nil
	}
	roots := map[string]string{}
	if list, err := s.ws.List(ctx); err == nil {
		for _, ws := range list {
			roots[ws.ID] = filepath.Clean(ws.Path)
		}
	}
	lr := make([]job.LayoutRow, len(rows))
	var out []alerts.DoctorFinding
	for i, r := range rows {
		lr[i] = job.LayoutRow{WorkspaceID: r.WorkspaceID, Name: r.Name, Path: r.Path, Kind: r.Kind, Owner: r.OwnerIndex()}
		if r.Kind != "job" || job.OwnerOf(r.Name) != "" {
			continue
		}
		msg := fmt.Sprintf("Job %s is not in an agent. It does not run until you move it to an agent.", r.Name)
		if filepath.Clean(filepath.Dir(r.Path)) == roots[r.WorkspaceID] {
			msg = fmt.Sprintf("Job %s is at the top of its workspace, not in an agent. It does not run. Move its files by hand into a new folder inside the folder of an agent.", r.Name)
		}
		out = append(out, alerts.DoctorFinding{
			ID: job.CodeJobOutsideAgent + ":" + r.Path, Severity: "error", Code: job.CodeJobOutsideAgent, Message: msg,
		})
	}
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
	events, err := s.alerts.ListAlerts(context.Background(), time.Now().Add(-7*24*time.Hour), 200, "")
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

// assistantDoctor adapts the session manager's doctor findings.
func (s *Server) assistantDoctor() []alerts.DoctorFinding {
	if s.asstDoctor == nil {
		return nil
	}
	findings := s.asstDoctor.Findings(context.Background())
	out := make([]alerts.DoctorFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, alerts.DoctorFinding{
			ID: f.ID, Severity: f.Severity, Code: f.Code, Message: f.Message,
		})
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

// handleSystemUpdate reads the latest trebi GitHub release and looks for the
// trebi_* asset for this platform. trebi-pro publishes that release.
func (s *Server) handleSystemUpdate(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"current": Version, "latest": Version, "update_available": false, "source": "github",
	}
	tag, url, err := s.latestRelease(r.Context())
	if err != nil || tag == "" {
		writeJSON(w, http.StatusOK, body)
		return
	}
	latest := strings.TrimPrefix(tag, "v")
	body["latest"] = latest
	if url != "" && versionNewer(latest, Version) {
		body["update_available"] = true
		body["url"] = url
	}
	writeJSON(w, http.StatusOK, body)
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

// handleCloudLink mints the device keypair, registers it with the code
// the account page showed, writes the link row, and starts the socket
// (plan 2026-09-22 Part G). The code arrives with the display grouping
// the account page renders, so it is normalized here.
func (s *Server) handleCloudLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EnrollmentToken string `json:"enrollment_token"`
		Name            string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "body is not JSON")
		return
	}
	token := normalizeEnrollToken(body.EnrollmentToken)
	if token == "" {
		writeErr(w, http.StatusBadRequest, "validation", "enrollment_token is required")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name, _ = os.Hostname()
	}
	st, err := s.cloud.Link(r.Context(), token, name)
	var apiErr *cloudlink.APIError
	switch {
	case errors.Is(err, cloudlink.ErrAlreadyLinked):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.As(err, &apiErr) && apiErr.Code == cloudlink.CodeMachineLimit:
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", apiErr.Message)
	case err != nil:
		writeCloudErr(w, err)
	default:
		writeJSON(w, http.StatusOK, st)
	}
}

// handleCloudLinkRequest links this machine by email: the cloud mails a
// confirm link and the daemon waits in the background. It answers 202 with
// the pending status.
func (s *Server) handleCloudLinkRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "body is not JSON")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = computerName()
	}
	st, err := s.cloud.RequestLink(r.Context(), body.Email, name)
	if err != nil {
		writeCloudLinkErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

// handleCloudLinkResend sends the confirm mail again.
func (s *Server) handleCloudLinkResend(w http.ResponseWriter, r *http.Request) {
	st, err := s.cloud.ResendLink(r.Context())
	if err != nil {
		writeCloudLinkErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleCloudLinkCancel drops the open link request.
func (s *Server) handleCloudLinkCancel(w http.ResponseWriter, r *http.Request) {
	if _, err := s.cloud.CancelLink(r.Context()); err != nil {
		writeCloudLinkErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeCloudLinkErr maps the errors of the link by email calls.
func writeCloudLinkErr(w http.ResponseWriter, err error) {
	var (
		apiErr *cloudlink.APIError
		retry  *cloudlink.RetryAfterError
	)
	switch {
	case errors.Is(err, cloudlink.ErrAlreadyLinked):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, cloudlink.ErrInvalidEmail):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, cloudlink.ErrNoPendingLink):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.As(err, &retry):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.After.Seconds()))))
		writeErrDetails(w, http.StatusTooManyRequests, "rate_limited", "Too many link requests. Try again later.",
			map[string]any{"retry_after_ms": retry.After.Milliseconds()})
	case errors.As(err, &apiErr) && apiErr.Code == "validation":
		writeErr(w, http.StatusBadRequest, "validation", apiErr.Message)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", cloudlink.ErrNoPendingLink.Error())
	default:
		writeCloudErr(w, err)
	}
}

// computerName is the name of this machine for the cloud account page:
// the Mac computer name, or the host name.
func computerName() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "scutil", "--get", "ComputerName").Output(); err == nil {
			if n := strings.TrimSpace(string(out)); n != "" {
				return n
			}
		}
	}
	name, _ := os.Hostname()
	return name
}

// normalizeEnrollToken strips the display grouping the account page
// shows (uppercase 4-character groups joined by "-") and keeps the code
// itself.
func normalizeEnrollToken(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(v)) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// handleCloudUnlink stops the socket, drops the row and the key, and
// leaves the device in the cloud: a local unlink must work offline and
// the account page revokes (Part G). The confirm gate wraps it.
func (s *Server) handleCloudUnlink(w http.ResponseWriter, r *http.Request) {
	if err := s.cloud.Unlink(r.Context()); err != nil {
		writeCloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.cloud.Status())
}

// handleCloudAddresses returns the hosted address rows, masked unless
// reveal=1 (Part G). A reveal is an audit entry like the workspace email
// reveal: the request log records who asked.
func (s *Server) handleCloudAddresses(w http.ResponseWriter, r *http.Request) {
	rows, err := s.cloud.Addresses(r.Context())
	if err != nil {
		writeCloudErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, maskedBody(s, r, "addresses", "cloud.address_revealed", rows, cloudAddressPayload))
}

// cloudAddressPayload is the wire shape for one hosted address row
// (trebi-ui cloudAddressSchema).
func cloudAddressPayload(a cloudlink.Address, masked bool) map[string]any {
	addr, local := a.Address, a.LocalPart
	if masked {
		local = maskLocalPart(local)
		addr = maskAddress(a.Address)
	}
	return map[string]any{
		"id":           a.ID,
		"workspace_id": a.WorkspaceID,
		"address":      addr,
		"local_part":   local,
		"created_at":   a.CreatedAt,
		"masked":       masked,
	}
}

// maskLocalPart hides the token and keeps the reading aid prefix.
func maskLocalPart(lp string) string {
	if len(lp) > 3 {
		lp = lp[:3]
	}
	return lp + "********"
}

// maskAddress hides the token in local@domain and keeps the shape.
func maskAddress(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return "********"
	}
	return maskLocalPart(local) + "@" + domain
}
