package alerts_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

// idRE matches the IDs and the developer keys that an alert must not show.
var idRE = regexp.MustCompile(`(?i)[0-9a-hjkmnp-tv-z]{26}|con_|ws_|jb_|agt_|agents/|kind:|inbox:|run:`)

const (
	nameWS   = "ws_c96eb452"
	nameRun  = "01M6MSH24QD4B58P7HVGXK43R5"
	nameConn = "con_01m3sm9y9t1bqg50025qjgmvzn"
)

// TestAlertTextsHaveNoIDs raises each kind with refs only. No title, body,
// hint, mail subject, or mail text shows an ID. Only the link has one.
func TestAlertTextsHaveNoIDs(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	svc, st := nameSvc(t, w)
	ctx := context.Background()
	job := alerts.RaiseOpts{WorkspaceID: nameWS, JobID: "jb_0000000001", RunID: nameRun}
	for _, k := range []alerts.Kind{
		alerts.KindWaiting, alerts.KindRunFailed, alerts.KindMemoryInvalid, alerts.KindResumeExhausted,
		alerts.KindActionsInvalid, alerts.KindRequest, alerts.KindRequestUndelivered, alerts.KindTriggerFlood,
		alerts.KindChainLimited, alerts.KindArtifactMissing, alerts.KindSkillCreated, alerts.KindJobStalled,
		alerts.KindRunStuck, alerts.KindSkipBlocked, alerts.KindRequestReminder, alerts.KindBudgetExceeded,
		alerts.KindAgentEscalation, alerts.KindAgentProposal,
	} {
		o := job
		o.Kind, o.Subject = k, "test/"+string(k)
		if _, err := svc.Raise(ctx, o); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	for _, k := range []alerts.Kind{
		alerts.KindConnectorUnhealthy, alerts.KindConnectorAuthRequired, alerts.KindConnectorUpdate,
		alerts.KindConnectorSubscriptionFailed, alerts.KindConnectorSubscriptionAction,
	} {
		if _, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: k, Connector: nameConn}); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	for _, k := range []alerts.Kind{alerts.KindAuthMissing, alerts.KindLimitExceeded, alerts.KindUsageWarning, alerts.KindHarnessUpdateFailed} {
		if _, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: k, Harness: "claude"}); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	for _, ev := range []store.BusEvent{
		{Topic: "run.failed", Entity: "run", EntityID: nameRun, Action: "updated", Attrs: map[string]string{
			"end_reason": "failed", "status": "failed", "job_id": "jb_0000000001", "workspace": nameWS}},
		{Topic: "connector.auth.required", Entity: "connector", EntityID: nameConn, Action: "updated", Attrs: map[string]string{
			"instance": nameConn, "reason": "logged_out", "message": "the phone removed this linked device"}},
	} {
		if _, err := st.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	settle(ctx, svc)

	items, err := listAll(ctx, svc, alerts.ListOpts{})
	if err != nil || len(items) == 0 {
		t.Fatalf("items %v %v", items, err)
	}
	for _, it := range items {
		full, err := svc.Get(ctx, it.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{full.Title, full.Body, full.Hint} {
			if m := idRE.FindString(text); m != "" {
				t.Errorf("%s: %q shows %q", full.AlertKind, text, m)
			}
		}
	}
	sends := w.sent()
	if len(sends) == 0 {
		t.Fatal("no mail")
	}
	for _, m := range sends {
		if !strings.HasPrefix(m.Subject, "[Trebi] ") {
			t.Errorf("subject %q has no tag", m.Subject)
		}
		for _, line := range append([]string{m.Subject}, strings.Split(m.Text, "\n")...) {
			if strings.Contains(line, "https://trebi.example.com/") {
				continue
			}
			if id := idRE.FindString(line); id != "" {
				t.Errorf("%s: line %q shows %q", m.Subject, line, id)
			}
		}
	}
	want := map[string]bool{
		"[Trebi] Car Hunt failed":                          false,
		"[Trebi] WhatsApp Ellis needs you to log in again": false,
		"[Trebi] Claude Code needs you to log in":          false,
		"[Trebi] A run of Car Hunt does not end":           false,
		"[Trebi] Update available for WhatsApp Ellis":      false,
	}
	for _, m := range sends {
		if _, ok := want[m.Subject]; ok {
			want[m.Subject] = true
			if !strings.Contains(m.Text, "Ellis · Car Hunt · btd") && !strings.Contains(m.Text, "WhatsApp Ellis") &&
				!strings.Contains(m.Text, "Claude Code") {
				t.Errorf("%s: no details line in %q", m.Subject, m.Text)
			}
		}
	}
	for subject, seen := range want {
		if !seen {
			t.Errorf("no mail %q", subject)
		}
	}
}

// nameSvc has the agent Ellis with the job Car Hunt in the workspace btd,
// and one run of Car Hunt.
func nameSvc(t *testing.T, w *sendWorker) (*alerts.Service, *store.Store) {
	t.Helper()
	svc, st := routeSvc(t, alerts.Options{
		PublicURL: "https://trebi.example.com/",
		ConnectionTitle: func(ref string) string {
			if ref == nameConn {
				return "WhatsApp Ellis"
			}
			return ""
		},
	}, nil, w, nil)
	dir := t.TempDir()
	files := map[string]string{
		"agents/ellis/AGENT.md":        "---\ntitle: Ellis\n---\n\nHelp.\n",
		"agents/ellis/car-hunt/JOB.md": "---\ntitle: Car Hunt\n---\n\nFind cars.\n",
		"ellis/car-hunt/.gitignore":    "",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES (?,'btd',?,0,datetime(),datetime())`,
			[]any{nameWS, dir}},
		{`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001',?,'ellis',?,'agent',1,'claude','h',datetime())`,
			[]any{nameWS, filepath.Join(dir, "agents/ellis/AGENT.md")}},
		{`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('jb_0000000001',?,'ellis/car-hunt',?,'job',1,'claude','h',datetime())`,
			[]any{nameWS, filepath.Join(dir, "agents/ellis/car-hunt/JOB.md")}},
		{`INSERT INTO runs (id,workspace_id,job_id,kind,reason,status,end_reason,harness,trace_id,started_at,updated_at)
			VALUES (?,?,'jb_0000000001','job','schedule','failed','failed','claude','trace',?,?)`,
			[]any{nameRun, nameWS, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)}},
	} {
		if _, err := st.DB().Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return svc, st
}
