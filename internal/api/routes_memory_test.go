package api_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
)

// memValid is a valid MEMORY.md for PUT bodies.
const memValid = `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context

Client sends CSV.

## Lessons

## Log

- [2026-09-14 09:00] ran the digest
`

// memBroken misses a section.
const memBroken = `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context

Client sends CSV.

## Log
`

// TestMemoryGetPutValidate covers the file plane: GET on a missing file, PUT
// with and without force, and the validate endpoint.
func TestMemoryGetPutValidate(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/memo")
	wsID := addWorkspace(t, base, wsDir, "mem-ws")
	root, q := base+"/v1/jobs/ops%2Fmemo/memory", "?workspace="+wsID

	// Missing file: skeleton content, zero size, empty hash, no issues.
	code, mem := doReq(t, "GET", root+q, nil, nil)
	if code != 200 {
		t.Fatalf("get missing %d %v", code, mem)
	}
	if v, _ := mem["size_bytes"].(float64); v != 0 {
		t.Fatalf("missing size_bytes = %v", mem["size_bytes"])
	}
	if mem["hash"] != "" {
		t.Fatalf("missing hash = %v", mem["hash"])
	}
	if !strings.Contains(mem["content"].(string), "## Context") {
		t.Fatalf("missing-file content: %v", mem["content"])
	}
	if issues, _ := mem["issues"].([]any); len(issues) != 0 {
		t.Fatalf("missing-file issues: %v", issues)
	}
	if _, ok := mem["legacy"]; ok {
		t.Fatalf("legacy field is back: %v", mem)
	}

	// PUT valid content.
	code, body := doReq(t, "PUT", root+q, nil, map[string]any{"content": memValid})
	if code != 200 {
		t.Fatalf("put %d %v", code, body)
	}
	code, mem = doReq(t, "GET", root+q, nil, nil)
	if code != 200 || mem["content"] != memValid {
		t.Fatalf("get after put: %d %v", code, mem["content"])
	}
	hash, _ := mem["hash"].(string)
	if hash == "" {
		t.Fatalf("no hash: %v", mem)
	}
	if v, _ := mem["size_bytes"].(float64); v == 0 {
		t.Fatalf("size_bytes = 0")
	}

	// PUT invalid content without force: 422 with issues; file unchanged.
	code, body = doReq(t, "PUT", root+q, nil, map[string]any{"content": memBroken})
	if code != 422 || errCode(body) != "validation" {
		t.Fatalf("broken put %d %v", code, body)
	}
	_, mem = doReq(t, "GET", root+q, nil, nil)
	if mem["content"] != memValid {
		t.Fatalf("broken put changed file")
	}

	// PUT invalid content with force=1: writes anyway.
	code, body = doReq(t, "PUT", root+q+"&force=1", nil, map[string]any{"content": memBroken})
	if code != 200 {
		t.Fatalf("forced put %d %v", code, body)
	}

	// Validate body content; never writes.
	code, body = doReq(t, "POST", root+"/validate"+q, nil,
		map[string]any{"content": memValid})
	if _, ok := body["legacy"]; code != 200 || body["valid"] != true || ok {
		t.Fatalf("validate valid: %d %v", code, body)
	}
	code, body = doReq(t, "POST", root+"/validate"+q, nil,
		map[string]any{"content": "Old notes.\n\n[2026-09-01] First.\n"})
	if code != 200 || body["valid"] != false {
		t.Fatalf("validate old ledger: %d %v", code, body)
	}
	_, mem = doReq(t, "GET", root+q, nil, nil)
	if mem["content"] != memBroken {
		t.Fatalf("validate wrote the file")
	}
	// Validate the disk file (empty body).
	code, body = doReq(t, "POST", root+"/validate"+q, nil, nil)
	if code != 200 || body["valid"] != false {
		t.Fatalf("validate disk: %d %v", code, body)
	}
}

// TestMemorySectionsAndEntries covers the section and entry routes.
func TestMemorySectionsAndEntries(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/memo")
	wsID := addWorkspace(t, base, wsDir, "mem-sec")
	root, q := base+"/v1/jobs/ops%2Fmemo/memory", "?workspace="+wsID

	code, body := doReq(t, "PUT", root+"/context"+q, nil,
		map[string]any{"body": "Client sends CSV nightly."})
	if code != 200 {
		t.Fatalf("put context %d %v", code, body)
	}
	// The State section route is gone (Part C).
	code, body = doReq(t, "PUT", root+"/state"+q, nil,
		map[string]any{"body": "Digest done."})
	if code < 400 {
		t.Fatalf("put state %d %v", code, body)
	}
	code, mem := doReq(t, "GET", root+q, nil, nil)
	if code != 200 {
		t.Fatalf("get %d", code)
	}
	doc, _ := mem["doc"].(map[string]any)
	if doc == nil || doc["context"] != "Client sends CSV nightly." {
		t.Fatalf("doc after sections: %v", doc)
	}

	// Lessons: add with explicit stamp, then delete by id.
	code, body = doReq(t, "POST", root+"/lessons"+q, nil,
		map[string]any{"text": "CSV comes zipped", "at": "2026-09-12 08:00"})
	if code != 201 {
		t.Fatalf("lesson add %d %v", code, body)
	}
	lid, _ := body["id"].(string)
	if lid == "" || body["at"] != "2026-09-12 08:00" {
		t.Fatalf("lesson payload: %v", body)
	}
	_, mem = doReq(t, "GET", root+q, nil, nil)
	doc, _ = mem["doc"].(map[string]any)
	if lessons, _ := doc["lessons"].([]any); len(lessons) != 1 {
		t.Fatalf("lesson not stored: %v", doc)
	}
	code, body = doReq(t, "DELETE", root+"/lessons/"+lid+q, nil, nil)
	if code != 200 {
		t.Fatalf("lesson delete %d %v", code, body)
	}
	code, body = doReq(t, "DELETE", root+"/lessons/ffffffff"+q, nil, nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown lesson %d %v", code, body)
	}

	// Log: stamp shape is enforced; entries land in the file.
	code, body = doReq(t, "POST", root+"/log"+q, nil,
		map[string]any{"text": "ran the digest", "at": "2026-09-14 09:00"})
	if code != 201 {
		t.Fatalf("log add %d %v", code, body)
	}
	eid, _ := body["id"].(string)
	code, body = doReq(t, "POST", root+"/log"+q, nil,
		map[string]any{"text": "bad stamp", "at": "09-14"})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("bad stamp %d %v", code, body)
	}
	code, body = doReq(t, "POST", root+"/log"+q, nil, map[string]any{"at": "2026-09-14 09:00"})
	if code != 400 {
		t.Fatalf("empty text %d %v", code, body)
	}
	data, err := os.ReadFile(filepath.Join(wsDir, "agents", "ops", "memo", "MEMORY.md"))
	if err != nil || !strings.Contains(string(data), "- [2026-09-14 09:00] ran the digest") {
		t.Fatalf("log line missing: %v %q", err, data)
	}
	code, body = doReq(t, "DELETE", root+"/log/"+eid+q, nil, nil)
	if code != 200 {
		t.Fatalf("log delete %d %v", code, body)
	}
}

// TestMemoryLogOverCap: a log add that crosses max_logs returns 422
// log_over_cap and writes nothing.
func TestMemoryLogOverCap(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	dir := filepath.Join(wsDir, "agents", "ops", "memo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "JOB.md"),
		[]byte("---\nharness: fake\nmemory:\n  max_logs: 2\n---\n\nDo work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "mem-cap")
	root, q := base+"/v1/jobs/ops%2Fmemo/memory", "?workspace="+wsID

	for i, text := range []string{"one", "two"} {
		code, body := doReq(t, "POST", root+"/log"+q, nil,
			map[string]any{"text": text, "at": "2026-09-14 09:0" + string(rune('0'+i))})
		if code != 201 {
			t.Fatalf("log add %d: %d %v", i, code, body)
		}
	}
	code, body := doReq(t, "POST", root+"/log"+q, nil,
		map[string]any{"text": "three", "at": "2026-09-14 09:05"})
	if code != 422 || errCode(body) != "log_over_cap" {
		t.Fatalf("over cap %d %v", code, body)
	}
	_, mem := doReq(t, "GET", root+q, nil, nil)
	doc, _ := mem["doc"].(map[string]any)
	if log, _ := doc["log"].([]any); len(log) != 2 {
		t.Fatalf("over-cap write leaked: %v", doc["log"])
	}
	// The job options shaped the payload.
	if opt, _ := mem["options"].(map[string]any); opt["max_logs"] != float64(2) {
		t.Fatalf("options = %v", mem["options"])
	}
}

// TestMemoryMigrateRouteGone: the migrate route is removed.
func TestMemoryMigrateRouteGone(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/memo")
	wsID := addWorkspace(t, base, wsDir, "mem-mig")
	code, _ := doReq(t, "POST", base+"/v1/jobs/ops%2Fmemo/memory/migrate?workspace="+wsID, nil, nil)
	if code != 404 {
		t.Fatalf("migrate %d, want 404", code)
	}
}

// TestMemoryReset: DELETE rewrites the skeleton behind the confirm gate.
func TestMemoryReset(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/memo")
	wsID := addWorkspace(t, base, wsDir, "mem-reset")
	root, q := base+"/v1/jobs/ops%2Fmemo/memory", "?workspace="+wsID

	if code, body := doReq(t, "POST", root+"/log"+q, nil,
		map[string]any{"text": "one event"}); code != 201 {
		t.Fatalf("log add %d %v", code, body)
	}
	code, body := doReq(t, "DELETE", root, nil, nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("reset without confirm %d %v", code, body)
	}
	code, body = doReq(t, "DELETE", root+q+"&confirm=1", nil, nil)
	if code != 200 {
		t.Fatalf("reset %d %v", code, body)
	}
	code, mem := doReq(t, "GET", root+q, nil, nil)
	if code != 200 || !strings.Contains(mem["content"].(string), "## Context") {
		t.Fatalf("get after reset: %d %v", code, mem)
	}
	doc, _ := mem["doc"].(map[string]any)
	if log, _ := doc["log"].([]any); len(log) != 0 {
		t.Fatalf("reset kept entries: %v", doc["log"])
	}
}

// TestMemoryIfMatch covers B2 on the memory sub-resource.
func TestMemoryIfMatch(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/memo")
	wsID := addWorkspace(t, base, wsDir, "memifm-ws")
	root, q := base+"/v1/jobs/ops%2Fmemo/memory", "?workspace="+wsID

	code, _ := doReq(t, "PUT", root+q, nil, map[string]any{"content": memValid})
	if code != 200 {
		t.Fatalf("seed put %d", code)
	}
	code, mem := doReq(t, "GET", root+q, nil, nil)
	if code != 200 {
		t.Fatalf("get %d", code)
	}
	hash, _ := mem["hash"].(string)

	// Stale If-Match → 412
	code, body := doReq(t, "PUT", root+q,
		map[string]string{"If-Match": "deadbeef"}, map[string]any{"content": memValid})
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("want 412 got %d %v", code, body)
	}

	// Fresh If-Match → 200
	code, body = doReq(t, "PUT", root+q,
		map[string]string{"If-Match": hash}, map[string]any{"content": memValid})
	if code != 200 {
		t.Fatalf("fresh put %d %v", code, body)
	}

	// Ambiguous job name on memory route → 400
	ws2 := t.TempDir()
	writeJobFile(t, ws2, "ops/memo")
	_ = addWorkspace(t, base, ws2, "memifm-ws2")
	code, body = doReq(t, "GET", base+"/v1/jobs/ops%2Fmemo/memory", nil, nil)
	if code != 400 || errCode(body) != "ambiguous" {
		t.Fatalf("want 400 ambiguous got %d %v", code, body)
	}
}

// TestAssistantMemoryFallback: with no assistant row, the assistant memory
// route serves the workspace-root MEMORY.md.
func TestAssistantMemoryFallback(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"), []byte("---\n---\n\nHelp.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "mem-asst")

	code, mem := doReq(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant/memory", nil, nil)
	if code != 200 || !strings.Contains(mem["content"].(string), "## Context") {
		t.Fatalf("assistant memory get: %d %v", code, mem)
	}
	if opt, _ := mem["options"].(map[string]any); opt["max_logs"] != float64(100) {
		t.Fatalf("assistant options = %v", mem["options"])
	}
}

// TestMemoryDoctor covers the recent memory_invalid alert finding.
func TestMemoryDoctor(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/memo")
	_ = addWorkspace(t, base, wsDir, "mem-doc-ws")

	// No memory files and no alerts: no memory findings.
	if codes := doctorCodes(t, s, base); hasCode(codes, "memory_invalid_recent") {
		t.Fatalf("unexpected memory findings %v", codes)
	}

	// A memory_invalid alert inside the window: one summary finding.
	if _, err := s.Alerts().Raise(context.Background(), alerts.RaiseOpts{
		Kind: alerts.KindMemoryInvalid, Subject: "ops/memo", Title: "MEMORY.md invalid",
	}); err != nil {
		t.Fatal(err)
	}
	if codes := doctorCodes(t, s, base); !hasCode(codes, "memory_invalid_recent") {
		t.Fatalf("want memory_invalid_recent in %v", codes)
	}
}

// doctorCodes pulls the code list out of a doctor response.
func doctorCodes(t *testing.T, s *api.Server, base string) []string {
	t.Helper()
	code, body := doReq(t, "GET", base+"/v1/system/doctor", nil, nil)
	if code != 200 {
		t.Fatalf("doctor %d %v", code, body)
	}
	items, _ := body["findings"].([]any)
	var out []string
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m["code"].(string))
		}
	}
	return out
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}
