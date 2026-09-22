package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestHandleRunLogJSONAndFollowSnapshot(t *testing.T) {
	_, base := startTestServer(t)
	id := "01TESTLOG0000000000000001"
	dir := home.RunsLogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&b, "L%03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/runs/"+id+"/log", nil)
	if code != 200 {
		t.Fatalf("json status %d %v", code, body)
	}
	log, _ := body["log"].(string)
	if !strings.Contains(log, "L001") || !strings.Contains(log, "L150") {
		t.Fatalf("json log missing ends: %q", log[:min(80, len(log))])
	}
	if body["source"] != "file" {
		t.Fatalf("source %v", body["source"])
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/runs/"+id+"/log?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("follow status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	sse := string(raw)
	if !strings.Contains(sse, "event: ready") {
		t.Fatalf("missing ready: %q", sse[:min(200, len(sse))])
	}
	if !strings.Contains(sse, `"offset":50`) || !strings.Contains(sse, `"total":150`) {
		t.Fatalf("ready metadata: %q", sse[:min(200, len(sse))])
	}
	if strings.Contains(sse, "L001") || strings.Contains(sse, "L050") {
		t.Fatalf("snapshot should be last 100 lines, got early lines: %q", sse[:min(300, len(sse))])
	}
	if !strings.Contains(sse, "data: L051") || !strings.Contains(sse, "data: L150") {
		t.Fatalf("missing tail lines: %q", sse[max(0, len(sse)-400):])
	}
}

func TestHandleRunLogFollowMissingIsSSENot404(t *testing.T) {
	_, base := startTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/runs/01MISSING0000000000000001/log?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "event: ready") {
		t.Fatalf("body %q", raw)
	}

	code, _, _ := doJSON(t, "GET", base+"/v1/runs/01MISSING0000000000000001/log", nil)
	if code != 404 {
		t.Fatalf("non-follow want 404 got %d", code)
	}
}

func TestHandleRunLogBeforePage(t *testing.T) {
	_, base := startTestServer(t)
	id := "01TESTLOGPAGE000000000001"
	dir := home.RunsLogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&b, "L%03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/runs/"+id+"/log?before=50&n=3", nil)
	if code != 200 {
		t.Fatalf("status %d %v", code, body)
	}
	log, _ := body["log"].(string)
	if log != "L048\nL049\nL050" {
		t.Fatalf("page log %q", log)
	}
	if body["offset"] != float64(47) || body["total"] != float64(150) {
		t.Fatalf("offset/total %v %v", body["offset"], body["total"])
	}
}
