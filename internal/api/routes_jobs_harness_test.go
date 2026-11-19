package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

// TestJobListHarnessSource covers the resolved harness of the list and the
// harness filter on it.
func TestJobListHarnessSource(t *testing.T) {
	_, base, _ := startAPI(t)
	_, wsID := harnessWorkspace(t, base)

	jobs := listJobs(t, base, "workspace="+wsID)
	plain, pinned := jobs["ops/plain"], jobs["ops/pinned"]
	if plain["harness"] != "codex" || plain["harness_source"] != job.ChoiceAgent {
		t.Fatalf("plain: %v", plain)
	}
	if pinned["harness"] != "claude" || pinned["harness_source"] != job.ChoiceJob {
		t.Fatalf("pinned: %v", pinned)
	}
	got := listJobs(t, base, "workspace="+wsID+"&harness=codex")
	if len(got) != 1 || got["ops/plain"] == nil {
		t.Fatalf("harness=codex: %v", got)
	}
}

// TestJobDetailResolvedChoice covers the resolved harness, model, and effort
// of the job detail, each with its source.
func TestJobDetailResolvedChoice(t *testing.T) {
	_, base, _ := startAPI(t)
	_, wsID := harnessWorkspace(t, base)

	code, plain, _ := doJSON(t, "GET", base+"/v1/jobs/ops%2Fplain?workspace="+wsID, nil)
	if code != http.StatusOK {
		t.Fatalf("get plain %d %v", code, plain)
	}
	for k, want := range map[string]string{
		"resolved_harness": "codex", "harness_source": job.ChoiceAgent,
		"resolved_model": "gpt-5", "model_source": job.ChoiceAgent,
		"effort_source": job.ChoiceDefault,
	} {
		if plain[k] != want {
			t.Fatalf("plain %s=%v want %q: %v", k, plain[k], want, plain)
		}
	}

	// The agent model is for codex, so a claude job does not take it.
	code, pinned, _ := doJSON(t, "GET", base+"/v1/jobs/ops%2Fpinned?workspace="+wsID, nil)
	if code != http.StatusOK {
		t.Fatalf("get pinned %d %v", code, pinned)
	}
	if pinned["resolved_harness"] != "claude" || pinned["harness_source"] != job.ChoiceJob {
		t.Fatalf("pinned harness: %v", pinned)
	}
	if pinned["model_source"] != job.ChoiceDefault || pinned["resolved_model"] == "gpt-5" {
		t.Fatalf("pinned model: %v %v", pinned["resolved_model"], pinned["model_source"])
	}
}

// TestJobHarnessNodeOmitted covers create with no harness and a patch that
// clears the harness: JOB.md has no harness key, so the agent value applies.
func TestJobHarnessNodeOmitted(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir, wsID := harnessWorkspace(t, base)

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "fresh", "agent": "ops", "prompt": "Go.",
	})
	if code != http.StatusCreated {
		t.Fatalf("create %d %v", code, body)
	}
	readJob := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(wsDir, "agents", "ops", name, "JOB.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if md := readJob("fresh"); strings.Contains(md, "harness:") {
		t.Fatalf("create wrote a harness node:\n%s", md)
	}

	for _, clear := range []any{nil, ""} {
		code, body, _ = doJSON(t, "PATCH", base+"/v1/jobs/ops%2Fpinned?workspace="+wsID, map[string]any{"harness": "claude"})
		if code != http.StatusOK || !strings.Contains(readJob("pinned"), "harness: claude") {
			t.Fatalf("set harness %d %v", code, body)
		}
		code, body, _ = doJSON(t, "PATCH", base+"/v1/jobs/ops%2Fpinned?workspace="+wsID, map[string]any{"harness": clear})
		if code != http.StatusOK {
			t.Fatalf("clear harness %#v: %d %v", clear, code, body)
		}
		if md := readJob("pinned"); strings.Contains(md, "harness:") {
			t.Fatalf("clear harness %#v left the node:\n%s", clear, md)
		}
		if got := listJobs(t, base, "workspace="+wsID)["ops/pinned"]; got["harness"] != "codex" || got["harness_source"] != job.ChoiceAgent {
			t.Fatalf("clear harness %#v: %v", clear, got)
		}
	}
}

// TestAgentEditUpdatesJobs covers an AGENT.md edit: each job of the agent
// gets job.updated, and the list shows the new harness.
func TestAgentEditUpdatesJobs(t *testing.T) {
	_, base, dir := startAPI(t)
	_, wsID := harnessWorkspace(t, base)
	plainID, _ := listJobs(t, base, "workspace="+wsID)["ops/plain"]["id"].(string)
	if plainID == "" {
		t.Fatal("plain job has no id")
	}
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	updates := func() int {
		t.Helper()
		evs, err := st.ListBus(t.Context(), 0, []string{"job.updated"}, 500)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ev := range evs {
			if ev.ID == plainID {
				n++
			}
		}
		return n
	}
	before := updates()

	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/ops", map[string]any{
		"content": "---\ndescription: Ops agent.\nharness: fake\n---\n\nRun ops.\n",
	})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("agent put %d %v", code, body)
	}
	if after := updates(); after <= before {
		t.Fatalf("job.updated for the agent job: before %d after %d", before, after)
	}
	if got := listJobs(t, base, "workspace="+wsID)["ops/plain"]; got["harness"] != "fake" || got["harness_source"] != job.ChoiceAgent {
		t.Fatalf("list after agent edit: %v", got)
	}
}

// harnessWorkspace has agent ops (harness codex, model gpt-5) with job
// "plain" (no harness) and job "pinned" (harness claude).
func harnessWorkspace(t *testing.T, base string) (string, string) {
	t.Helper()
	wsDir := t.TempDir()
	writeFiles(t, map[string]string{
		filepath.Join(wsDir, "agents", "ops", "AGENT.md"):         "---\ndescription: Ops agent.\nharness:\n  name: codex\n  model: gpt-5\n---\n\nRun ops.\n",
		filepath.Join(wsDir, "agents", "ops", "plain", "JOB.md"):  "---\ndescription: Plain job.\n---\n\nDo work.\n",
		filepath.Join(wsDir, "agents", "ops", "pinned", "JOB.md"): "---\nharness: claude\n---\n\nDo work.\n",
	})
	return wsDir, addWorkspace(t, base, wsDir, "harness-ws")
}

func listJobs(t *testing.T, base, query string) map[string]map[string]any {
	t.Helper()
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?"+query, nil)
	if code != http.StatusOK {
		t.Fatalf("list jobs %d %v", code, body)
	}
	out := map[string]map[string]any{}
	items, _ := body["jobs"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}
