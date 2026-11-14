package api_test

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// activityEnv holds one workspace with two agents (ops, cmo) and one job of ops.
type activityEnv struct {
	base, wsID          string
	opsID, cmoID, jobID string
	st                  *store.Store
	svc                 *runs.Service
	t                   *testing.T
}

// run stores one finished run of jobID with the given tokens.
func (e *activityEnv) run(jobID, name string, status runs.Status, input int64) string {
	e.t.Helper()
	run, err := e.svc.Create(e.t.Context(), runs.CreateOpts{WorkspaceID: e.wsID, JobID: jobID, JobName: name, Harness: "fake"})
	if err != nil {
		e.t.Fatal(err)
	}
	var usage *pricing.TokenUsage
	if input > 0 {
		usage = &pricing.TokenUsage{Input: input, Output: input / 10, Model: "default"}
	}
	if err := e.svc.Terminate(e.t.Context(), run.ID, status, runs.EndResult, usage); err != nil {
		e.t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	return run.ID
}

func TestStatsActivityRoute(t *testing.T) {
	e := newActivityEnv(t)
	e.run(e.jobID, "agents/ops/report", runs.StatusComplete, 1000)
	e.run(e.jobID, "agents/ops/report", runs.StatusFailed, 0)
	e.run(e.cmoID, "agents/cmo", runs.StatusComplete, 500)

	code, body, _ := doJSON(t, "GET", e.base+activityQuery(nil), nil)
	if code != 200 || body["group"] != "agent" || body["bin_ms"] != float64(900000) {
		t.Fatalf("activity: %d %v", code, body)
	}
	byKey := map[string]map[string]any{}
	for _, r := range body["rows"].([]any) {
		row := r.(map[string]any)
		byKey[row["key"].(string)] = row
	}
	sum := func(row map[string]any, field string) float64 {
		var n float64
		for _, b := range row["bins"].([]any) {
			n += b.(map[string]any)[field].(float64)
		}
		return n
	}
	ops, cmo := byKey[e.opsID], byKey[e.cmoID]
	if ops == nil || cmo == nil || len(byKey) != 2 {
		t.Fatalf("rows by agent: %v", byKey)
	}
	if sum(ops, "done") != 1 || sum(ops, "failed") != 1 || sum(ops, "tokens") != 1100 || sum(cmo, "done") != 1 {
		t.Fatalf("ops %v cmo %v", ops, cmo)
	}
	if _, ok := body["runs"]; ok {
		t.Errorf("the agent view lists no runs: %v", body["runs"])
	}

	_, body, _ = doJSON(t, "GET", e.base+activityQuery(url.Values{"group": {"job"}, "agent": {"ops"}}), nil)
	list, _ := body["runs"].([]any)
	if len(list) != 2 || len(body["rows"].([]any)) != 1 {
		t.Fatalf("job view of ops: %v", body)
	}
	if first := list[0].(map[string]any); first["job_id"] != e.jobID || first["state"] != "failed" {
		t.Errorf("newest run first: %v", first)
	}

	_, body, _ = doJSON(t, "GET", e.base+activityQuery(url.Values{"agent": {"nobody"}}), nil)
	if rows, _ := body["rows"].([]any); len(rows) != 0 {
		t.Errorf("unknown agent: %v", rows)
	}

	for name, extra := range map[string]url.Values{
		"missing from": {"from": {""}},
		"bad to":       {"to": {"tomorrow"}},
		"bad bin":      {"bin": {"soon"}},
		"bin too fine": {"bin": {"1m"}},
		"bad group":    {"group": {"team"}},
		"too long":     {"from": {time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339)}, "bin": {"4h"}},
	} {
		code, body, _ := doJSON(t, "GET", e.base+activityQuery(extra), nil)
		if code != 400 || errCode(body) != "validation" {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
}

func TestRunsListOwnerParam(t *testing.T) {
	e := newActivityEnv(t)
	own := e.run(e.opsID, "agents/ops", runs.StatusComplete, 0)
	viaJob := e.run(e.jobID, "agents/ops/report", runs.StatusComplete, 0)
	other := e.run(e.cmoID, "agents/cmo", runs.StatusComplete, 0)
	for _, ref := range []string{"ops", e.opsID, "agents/ops"} {
		code, body, _ := doJSON(t, "GET", e.base+"/v1/runs?owner="+url.QueryEscape(ref), nil)
		got := map[string]bool{}
		for _, r := range body["runs"].([]any) {
			got[r.(map[string]any)["id"].(string)] = true
		}
		if code != 200 || !got[own] || !got[viaJob] || got[other] || len(got) != 2 {
			t.Errorf("owner %q: %d %v", ref, code, got)
		}
	}
}

func TestJobsListRecentRunsAndTokens30d(t *testing.T) {
	e := newActivityEnv(t)
	var ids []string
	for range 16 {
		ids = append(ids, e.run(e.jobID, "agents/ops/report", runs.StatusComplete, 1000))
	}
	code, body, _ := doJSON(t, "GET", e.base+"/v1/jobs?workspace="+e.wsID, nil)
	if code != 200 {
		t.Fatalf("jobs: %d %v", code, body)
	}
	var row map[string]any
	for _, j := range body["jobs"].([]any) {
		if r := j.(map[string]any); r["id"] == e.jobID {
			row = r
		}
	}
	if row == nil {
		t.Fatalf("job row missing: %v", body)
	}
	if _, ok := row["mtd_tokens"]; ok {
		t.Errorf("mtd_tokens is gone: %v", row["mtd_tokens"])
	}
	tok := row["tokens_30d"].(map[string]any)
	if tok["billed"] != float64(16*1100) || tok["input"] != float64(16000) {
		t.Errorf("tokens_30d %v", tok)
	}
	recent := row["recent_runs"].([]any)
	if len(recent) != 14 {
		t.Fatalf("recent_runs %d, want 14", len(recent))
	}
	for i, r := range recent {
		bar := r.(map[string]any)
		if bar["id"] != ids[i+2] || bar["status"] != "complete" || bar["state"] != "done" {
			t.Fatalf("bar %d: %v (oldest first, newest 14)", i, bar)
		}
		if bar["tokens"].(map[string]any)["billed"] != float64(1100) {
			t.Errorf("bar %d tokens %v", i, bar["tokens"])
		}
	}
}

func TestAgentsListTokens30d(t *testing.T) {
	e := newActivityEnv(t)
	e.run(e.jobID, "agents/ops/report", runs.StatusComplete, 2000)
	e.run(e.opsID, "agents/ops", runs.StatusFailed, 1000)
	code, body, _ := doJSON(t, "GET", e.base+"/v1/workspaces/"+e.wsID+"/agents", nil)
	if code != 200 {
		t.Fatalf("agents: %d %v", code, body)
	}
	for _, a := range body["agents"].([]any) {
		row := a.(map[string]any)
		if _, ok := row["tokens_mtd"]; ok {
			t.Errorf("tokens_mtd is gone: %v", row["tokens_mtd"])
		}
		tok := row["tokens_30d"].(map[string]any)
		switch row["name"] {
		case "ops":
			if tok["billed"] != float64(3300) || row["runs_7d"] != float64(2) || row["ok_7d"] != float64(1) {
				t.Errorf("ops %v runs_7d %v ok_7d %v", tok, row["runs_7d"], row["ok_7d"])
			}
		case "cmo":
			if tok["billed"] != float64(0) {
				t.Errorf("cmo %v", tok)
			}
		}
	}
}

func newActivityEnv(t *testing.T) *activityEnv {
	t.Helper()
	s, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/ops/report")
	writeAgentFile(t, wsDir, "cmo")
	wsID := addWorkspace(t, base, wsDir, "activity")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	t.Cleanup(func() { _ = st.Close() })
	return &activityEnv{
		base: base, wsID: wsID, st: st, svc: s.Runs(), t: t,
		opsID: indexedJobID(t, st, wsID, "agents/ops"), cmoID: indexedJobID(t, st, wsID, "agents/cmo"),
		jobID: indexedJobID(t, st, wsID, "agents/ops/report"),
	}
}

func activityQuery(extra url.Values) string {
	q := url.Values{}
	now := time.Now().UTC()
	q.Set("from", now.Add(-time.Hour).Format(time.RFC3339))
	q.Set("to", now.Add(time.Hour).Format(time.RFC3339))
	q.Set("bin", "15m")
	for k, v := range extra {
		q[k] = v
	}
	return "/v1/stats/activity?" + q.Encode()
}
