package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestPlatformSurface_NonStub(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	client := &http.Client{Timeout: 30 * time.Second}
	auth := func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
	}

	// skills create + list
	req, _ := http.NewRequest("POST", base+"/v1/skills", bytes.NewBufferString(
		`{"name":"smoke-skill","skill_md":"# Smoke\n\n- bin: sh\n"}`))
	auth(req)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("skills create %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	req, _ = http.NewRequest("GET", base+"/v1/skills", nil)
	auth(req)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var skillsBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&skillsBody)
	resp.Body.Close()
	list, _ := skillsBody["skills"].([]any)
	if len(list) < 1 {
		t.Fatalf("skills empty: %v", skillsBody)
	}

	// secrets
	req, _ = http.NewRequest("PUT", base+"/v1/secrets/api-key", bytes.NewBufferString(`{"value":"secret"}`))
	auth(req)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("secret put %d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("GET", base+"/v1/secrets", nil)
	auth(req)
	resp, _ = client.Do(req)
	var sec map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&sec)
	resp.Body.Close()
	names, _ := sec["secrets"].([]any)
	if len(names) < 1 {
		t.Fatalf("secrets: %v", sec)
	}

	// harnesses
	req, _ = http.NewRequest("GET", base+"/v1/harnesses", nil)
	auth(req)
	resp, _ = client.Do(req)
	var har map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&har)
	resp.Body.Close()
	hs, _ := har["harnesses"].([]any)
	if len(hs) < 5 {
		t.Fatalf("harnesses: %v", har)
	}

	// doctor findings structure
	req, _ = http.NewRequest("GET", base+"/v1/system/doctor", nil)
	auth(req)
	resp, _ = client.Do(req)
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if _, ok := doc["findings"]; !ok {
		t.Fatalf("doctor: %v", doc)
	}

	// connectors upsert
	req, _ = http.NewRequest("POST", base+"/v1/connectors", bytes.NewBufferString(
		`{"slug":"line-src","from":"inline","manifest":{"schema":"trebi-connector/1","name":"line-src","title":"Line","description":"Lines","version":"1.0.0","events":{"command":"yes","format":"line"}}}`))
	auth(req)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("connector create %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("GET", base+"/v1/connectors", nil)
	auth(req)
	resp, _ = client.Do(req)
	var connector map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&connector)
	resp.Body.Close()
	ilist, _ := connector["connectors"].([]any)
	if len(ilist) < 1 {
		t.Fatalf("connectors: %v", connector)
	}

	// ensure skill folder on disk
	if _, err := os.Stat(filepath.Join(dir, "skills", "smoke-skill", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchRulesFireCreatesQueue(t *testing.T) {
	_, base := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}
	// workspace + job with event trigger so fire can match — rules/fire enqueues directly
	payload := `{"connector":"src","workspace":"` + fireWorkspace(t, base) + `","job":"agents/ops/j","match":"true","event":{"n":1}}`
	req, _ := http.NewRequest("POST", base+"/v1/rules/fire", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["outcome"] != "queued" {
		t.Fatalf("%v", body)
	}
	if body["event_id"] == nil || body["event_id"] == "" {
		t.Fatalf("expected durable event_id: %v", body)
	}
	req, _ = http.NewRequest("GET", base+"/v1/queue", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var q map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&q)
	items, _ := q["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("queue empty: %v", q)
	}
	// Durable events must be non-empty after rules/fire
	req, _ = http.NewRequest("GET", base+"/v1/events", nil)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var ev map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&ev)
	elist, _ := ev["events"].([]any)
	if len(elist) < 1 {
		t.Fatalf("events empty after rules/fire: %v", ev)
	}
}
