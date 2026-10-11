package api_test

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestInboxApplyValidation(t *testing.T) {
	s, base := startTestServer(t)
	id, err := s.Alerts().Raise(t.Context(), alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "u/1", Title: "usage"})
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, alerts.MaxApplyIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("id%d", i)
	}
	tests := []struct {
		name string
		body any
		code int
		err  string
	}{
		{"neither ids nor filter", map[string]any{"op": "read"}, 400, "validation"},
		{"ids and filter", map[string]any{"op": "read", "ids": []string{id}, "filter": map[string]any{}}, 400, "validation"},
		{"too many ids", map[string]any{"op": "read", "ids": many}, 400, "validation"},
		{"unknown op", map[string]any{"op": "resolve", "ids": []string{id}}, 400, "validation"},
		{"bad filter view", map[string]any{"op": "read", "filter": map[string]any{"view": "all"}}, 400, "validation"},
		{"malformed body", "not an object", 400, "validation"},
		{"unknown id", map[string]any{"op": "read", "ids": []string{"01MISSING"}}, 404, "not_found"},
	}
	for _, tt := range tests {
		code, body, _ := doJSON(t, "POST", base+"/v1/inbox/apply", tt.body)
		if code != tt.code || errCode(body) != tt.err {
			t.Fatalf("%s: %d %v", tt.name, code, body)
		}
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/inbox/apply", map[string]any{"op": "read", "ids": []string{id}})
	if updated, _ := body["updated"].([]any); code != 200 || len(updated) != 1 || updated[0] != id {
		t.Fatalf("read: %d %v", code, body)
	}
	_, item, _ := doJSON(t, "GET", base+"/v1/inbox/"+id, nil)
	if item["read"] != true || item["done"] != false || item["needs_action"] != false {
		t.Fatalf("item %v", item)
	}
	if _, ok := item["status"]; ok {
		t.Fatalf("item still has status: %v", item)
	}
	for _, q := range []string{"view=all", "kind=spam"} {
		if code, body, _ := doJSON(t, "GET", base+"/v1/inbox?"+q, nil); code != 400 || errCode(body) != "validation" {
			t.Fatalf("GET ?%s: %d %v", q, code, body)
		}
	}
}

func TestInboxApplyConfirm(t *testing.T) {
	s, base := startTestServer(t)
	ctx := t.Context()
	approval, err := s.Alerts().Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, Subject: "run/r1", RunID: "r1", Title: "Agent waiting"})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := s.Alerts().Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "u/1", Title: "usage"})
	if err != nil {
		t.Fatal(err)
	}
	req := map[string]any{"op": "archive", "ids": []string{approval, usage}}
	code, body, _ := doJSON(t, "POST", base+"/v1/inbox/apply", req)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("unconfirmed: %d %v", code, body)
	}
	e, _ := body["error"].(map[string]any)
	if msg, _ := e["message"].(string); msg != "1 item still waits for your answer. Archive it anyway?" {
		t.Fatalf("message %q", msg)
	}
	d := errDetails(body)
	pending, _ := d["pending"].([]any)
	if d["confirm_required"] != true || d["count"] != float64(1) || len(pending) != 1 {
		t.Fatalf("details %v", d)
	}
	if p, _ := pending[0].(map[string]any); p["id"] != approval || p["kind"] != "approval" || p["alert_kind"] != "waiting" || p["run_id"] != "r1" {
		t.Fatalf("pending %v", pending[0])
	}
	if _, page, _ := doJSON(t, "GET", base+"/v1/inbox", nil); len(page["items"].([]any)) != 2 {
		t.Fatalf("unconfirmed archive changed rows: %v", page)
	}

	req["confirm"] = true
	code, body, _ = doJSON(t, "POST", base+"/v1/inbox/apply", req)
	if updated, _ := body["updated"].([]any); code != 200 || len(updated) != 2 {
		t.Fatalf("confirmed: %d %v", code, body)
	}
	_, page, _ := doJSON(t, "GET", base+"/v1/inbox?view=done", nil)
	counts, _ := page["counts"].(map[string]any)
	if len(page["items"].([]any)) != 2 || counts["done"] != float64(2) || counts["inbox"] != float64(0) {
		t.Fatalf("done page %v", page)
	}
	unread, _ := counts["unread"].(map[string]any)
	if len(unread) != 6 || unread["approval"] != float64(0) {
		t.Fatalf("unread counts %v", unread)
	}
}

func TestDoctorFixByFinding(t *testing.T) {
	s, base := startTestServer(t)
	svc := s.Alerts()
	findings := []alerts.DoctorFinding{
		{ID: "harness-claude", Severity: "warn", Code: "harness_home_missing", Message: "no home", Fix: alerts.FixHarnessHome, Harness: "claude"},
		{ID: "harness-update-failed-claude", Severity: "warn", Code: "harness_update_failed", Message: "update failed"},
		{ID: "harness-kept-old-claude", Severity: "info", Code: "harness_kept_old", Message: "kept old"},
		{ID: "harness-off-ws1-agent", Severity: "warn", Code: "harness_off", Message: "off"},
	}
	svc.SetDoctor(func() []alerts.DoctorFinding { return findings })
	svc.SyncDoctor(t.Context(), findings)
	_ = os.RemoveAll(home.HarnessDir("claude"))

	tests := []struct {
		id   string
		code int
	}{
		{"harness-claude", 200},
		{"harness-update-failed-claude", 409},
		{"harness-kept-old-claude", 409},
		{"harness-off-ws1-agent", 409},
		{"no-such-finding", 409},
	}
	for _, tt := range tests {
		code, body, _ := doJSON(t, "POST", base+"/v1/system/doctor/"+tt.id+"/fix", nil)
		if code != tt.code {
			t.Fatalf("%s: %d %v", tt.id, code, body)
		}
		if tt.code == 409 {
			e, _ := body["error"].(map[string]any)
			if errCode(body) != "conflict" || e["message"] != "This finding has no automatic fix" {
				t.Fatalf("%s: %v", tt.id, body)
			}
		}
	}
	if _, err := os.Stat(home.HarnessDir("claude")); err != nil {
		t.Fatalf("harness home not made: %v", err)
	}
	_, page, _ := doJSON(t, "GET", base+"/v1/inbox?kind=health", nil)
	open := map[string]bool{}
	for _, raw := range page["items"].([]any) {
		it, _ := raw.(map[string]any)
		open[strings.TrimPrefix(it["subject"].(string), "doctor/")] = true
	}
	if open["harness-claude"] {
		t.Fatal("fixed finding stays open")
	}
	for _, id := range []string{"harness-update-failed-claude", "harness-kept-old-claude", "harness-off-ws1-agent"} {
		if !open[id] {
			t.Fatalf("%s closed without a fix (open %v)", id, open)
		}
	}
	_, doc, _ := doJSON(t, "GET", base+"/v1/system/doctor", nil)
	for _, raw := range doc["findings"].([]any) {
		f, _ := raw.(map[string]any)
		if want := f["id"] == "harness-claude"; (f["fix"] == alerts.FixHarnessHome) != want {
			t.Fatalf("doctor fix field %v", f)
		}
	}
	if code, _, _ := doJSON(t, http.MethodGet, base+"/v1/system/overview", nil); code != 200 {
		t.Fatalf("overview %d", code)
	}
}
