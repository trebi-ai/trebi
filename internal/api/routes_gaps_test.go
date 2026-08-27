package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryLedgerRoundTrip covers B1: entries write as [YYYY-MM-DD] ledger
// lines, GET returns the parsed wire shape, and delete removes one entry by id.
func TestMemoryLedgerRoundTrip(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "memo")
	wsID := addWorkspace(t, base, s.Token(), wsDir, "mem-ws")

	// POST first entry with explicit date
	code, ent := doReq(t, "POST", base+"/v1/jobs/memo/memory/entries?workspace="+wsID, s.Token(), nil,
		map[string]any{"date": "2026-01-02", "text": "did a thing"})
	if code != 201 {
		t.Fatalf("entry post %d %v", code, ent)
	}
	id1, _ := ent["id"].(string)
	if id1 == "" || ent["date"] != "2026-01-02" {
		t.Fatalf("bad entry: %v", ent)
	}
	memPath := filepath.Join(wsDir, "memo", "MEMORY.md")
	data, err := os.ReadFile(memPath)
	if err != nil || !strings.Contains(string(data), "[2026-01-02] did a thing") {
		t.Fatalf("ledger line missing: %v %q", err, data)
	}

	// POST second entry, date defaults to today
	code, ent2 := doReq(t, "POST", base+"/v1/jobs/memo/memory/entries?workspace="+wsID, s.Token(), nil,
		map[string]any{"text": "second note"})
	if code != 201 {
		t.Fatalf("entry post 2 %d %v", code, ent2)
	}

	// GET shows both entries + hash
	code, mem := doReq(t, "GET", base+"/v1/jobs/memo/memory?workspace="+wsID, s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("memory get %d %v", code, mem)
	}
	entries, _ := mem["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries: %v", mem)
	}
	if h, _ := mem["hash"].(string); h == "" {
		t.Fatalf("no hash: %v", mem)
	}

	// DELETE first entry by id
	code, del := doReq(t, "DELETE", base+"/v1/jobs/memo/memory/entries/"+id1+"?workspace="+wsID, s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("entry delete %d %v", code, del)
	}
	data, _ = os.ReadFile(memPath)
	if strings.Contains(string(data), "did a thing") || !strings.Contains(string(data), "second note") {
		t.Fatalf("delete wrong entry: %q", data)
	}

	// Delete of unknown id → 404
	code, body := doReq(t, "DELETE", base+"/v1/jobs/memo/memory/entries/ffffffff?workspace="+wsID, s.Token(), nil, nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("want 404 not_found got %d %v", code, body)
	}
}

// TestMemoryIfMatch covers B2 on the memory sub-resource.
func TestMemoryIfMatch(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "memo")
	wsID := addWorkspace(t, base, s.Token(), wsDir, "memifm-ws")

	code, _ := doReq(t, "POST", base+"/v1/jobs/memo/memory/entries?workspace="+wsID, s.Token(), nil,
		map[string]any{"text": "seed"})
	if code != 201 {
		t.Fatalf("seed entry %d", code)
	}
	code, mem := doReq(t, "GET", base+"/v1/jobs/memo/memory?workspace="+wsID, s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("get %d", code)
	}
	hash, _ := mem["hash"].(string)

	// Stale If-Match → 412
	code, body := doReq(t, "PUT", base+"/v1/jobs/memo/memory?workspace="+wsID, s.Token(),
		map[string]string{"If-Match": "deadbeef"}, map[string]any{"content": "reset\n"})
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("want 412 got %d %v", code, body)
	}

	// Fresh If-Match → 200
	code, body = doReq(t, "PUT", base+"/v1/jobs/memo/memory?workspace="+wsID, s.Token(),
		map[string]string{"If-Match": hash}, map[string]any{"content": "reset\n"})
	if code != 200 {
		t.Fatalf("fresh put %d %v", code, body)
	}

	// Ambiguous job name on memory route → 400
	ws2 := t.TempDir()
	writeJobFile(t, ws2, "memo")
	_ = addWorkspace(t, base, s.Token(), ws2, "memifm-ws2")
	code, body = doReq(t, "GET", base+"/v1/jobs/memo/memory", s.Token(), nil, nil)
	if code != 400 || errCode(body) != "ambiguous" {
		t.Fatalf("want 400 ambiguous got %d %v", code, body)
	}
}
