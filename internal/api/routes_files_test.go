package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestFilesCRUD(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "files-ws")

	body := []byte("print('hi')\n")
	code, resp, hdr := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/reports/render.py", nil, body)
	if code != 201 {
		t.Fatalf("put create %d %v", code, resp)
	}
	if resp["created"] != true || resp["path"] != "reports/render.py" {
		t.Fatalf("put body: %v", resp)
	}
	hash, _ := resp["hash"].(string)
	if hash == "" || hdr.Get("ETag") != `"`+hash+`"` {
		t.Fatalf("etag %q hash %q", hdr.Get("ETag"), hash)
	}

	code, resp, _ = doRaw(t, "GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?confirm=1", nil, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, resp)
	}

	req, _ := http.NewRequest("GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py", nil)
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if string(raw) != string(body) {
		t.Fatalf("body %q", raw)
	}
	if !strings.Contains(httpResp.Header.Get("Content-Type"), "python") &&
		httpResp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content-type %s", httpResp.Header.Get("Content-Type"))
	}

	req, _ = http.NewRequest("GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py", nil)
	req.Header.Set("If-None-Match", `"`+hash+`"`)
	httpResp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != 304 {
		t.Fatalf("if-none-match %d", httpResp.StatusCode)
	}

	code, stat, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?stat=1", nil)
	if code != 200 || stat["hash"] != hash {
		t.Fatalf("stat %d %v", code, stat)
	}

	code, list, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files?prefix=reports/", nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, list)
	}
	files, _ := list["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("list: %v", list)
	}

	code, _, _ = doRaw(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?confirm=1", nil, nil)
	if code != 200 {
		t.Fatalf("delete %d", code)
	}
	code, _, _ = doRaw(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?confirm=1", nil, nil)
	if code != 404 {
		t.Fatalf("delete missing %d", code)
	}
}

func TestFilesErrors(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "files-err")

	code, body, _ := doRaw(t, "PUT", base+"/v1/workspaces/no-such/files/a.py", nil, []byte("x"))
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("missing ws %d %v", code, body)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/%2e%2e%2f%2e%2e%2fx", nil, []byte("x"))
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("escape %d %v", code, body)
	}
	if msg := errMsg(body); strings.Contains(msg, wsDir) {
		t.Fatalf("echoed abs path: %s", msg)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/trebi.yaml", nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("trebi.yaml %d %v", code, body)
	}

	code, created, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/f.txt", nil, []byte("v1"))
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/f.txt",
		map[string]string{"If-Match": "deadbeef"}, []byte("v2"))
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("stale %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "current hash:") {
		t.Fatalf("412 message: %s", errMsg(body))
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/missing.txt",
		map[string]string{"If-Match": "*"}, []byte("x"))
	if code != 412 {
		t.Fatalf("star missing %d %v", code, body)
	}
}

func TestFilesOversize(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{FilesMaxBytes: 8})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "tiny")

	code, body, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/big.bin", nil, []byte("0123456789"))
	if code != 413 || errCode(body) != "validation" {
		t.Fatalf("oversize %d %v", code, body)
	}
}

func TestFilesJobMarkdownHook(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "files-job")
	md := []byte("---\nharness: fake\n---\n\nDo work.\n")

	code, created, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/reports/JOB.md", nil, md)
	if code != 201 {
		t.Fatalf("put job %d %v", code, created)
	}
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/%2Freports?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get job %d %v", code, got)
	}
	// The index writes the job ID into the file, so the hash covers the file on disk.
	onDisk, err := os.ReadFile(filepath.Join(wsDir, "reports", "JOB.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(onDisk), "---\nid: jb_") {
		t.Fatalf("job ID not written: %q", onDisk)
	}
	if got["hash"] != job.ContentHash(onDisk) {
		t.Fatalf("hash mismatch job=%v file=%s", got["hash"], job.ContentHash(onDisk))
	}

	frames := collectBus(t, base, "file.created,file.deleted,job.created,job.deleted", 600*time.Millisecond)
	if !hasTopic(frames, "file.created") || !hasTopic(frames, "job.created") {
		t.Fatalf("bus after create: %v", topicsOf(frames))
	}

	code, _, _ = doRaw(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/reports/JOB.md?confirm=1", nil, nil)
	if code != 200 {
		t.Fatalf("delete job %d", code)
	}
	frames = collectBus(t, base, "file.deleted,job.deleted", 600*time.Millisecond)
	if !hasTopic(frames, "file.deleted") || !hasTopic(frames, "job.deleted") {
		t.Fatalf("bus after delete: %v", topicsOf(frames))
	}
}

func TestFilesIdenticalJobWriteNoDupUpdate(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "files-dup")
	md := []byte("---\nharness: fake\n---\n\nSame.\n")
	url := base + "/v1/workspaces/" + wsID + "/files/dup/JOB.md"
	if code, _, _ := doRaw(t, "PUT", url, nil, md); code != 201 {
		t.Fatalf("first %d", code)
	}
	if code, _, _ := doRaw(t, "PUT", url, nil, md); code != 200 {
		t.Fatalf("second %d", code)
	}
	frames := collectBus(t, base, "job.created,job.updated", 600*time.Millisecond)
	nUpdated := 0
	for _, f := range frames {
		if f["topic"] == "job.updated" {
			nUpdated++
		}
	}
	if nUpdated != 0 {
		t.Fatalf("duplicate job.updated: %v", topicsOf(frames))
	}
}

func TestFileMutationIsAudited(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "audit-ws")
	code, _, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/a.py", nil, []byte("x"))
	if code != 201 {
		t.Fatalf("put %d (audit wrapper must not block raw PUT)", code)
	}
}

func TestJobFilesCRUD(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "job-files")
	createTestJob(t, base, wsID, "ops/report")
	createTestJob(t, base, wsID, "ops/other")

	body := []byte("print('hi')\n")
	code, resp, hdr := doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil, body)
	if code != 201 {
		t.Fatalf("put create %d %v", code, resp)
	}
	if resp["created"] != true || resp["path"] != "render.py" {
		t.Fatalf("put body (job-relative path): %v", resp)
	}
	hash, _ := resp["hash"].(string)
	if hash == "" || hdr.Get("ETag") != `"`+hash+`"` {
		t.Fatalf("etag %q hash %q", hdr.Get("ETag"), hash)
	}
	onDisk, err := os.ReadFile(filepath.Join(wsDir, "agents", "ops", "report", "render.py"))
	if err != nil || string(onDisk) != string(body) {
		t.Fatalf("disk: %q %v", onDisk, err)
	}

	code, _, _ = doRaw(t, "GET", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil, nil)
	if code != 200 {
		t.Fatalf("get %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil)
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if string(raw) != string(body) {
		t.Fatalf("body %q", raw)
	}

	req, _ = http.NewRequest("GET", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil)
	req.Header.Set("If-None-Match", `"`+hash+`"`)
	httpResp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != 304 {
		t.Fatalf("if-none-match %d", httpResp.StatusCode)
	}

	code, stat, _ := doJSON(t, "GET", base+"/v1/jobs/ops%2Freport/files/render.py?stat=1&workspace="+wsID, nil)
	if code != 200 || stat["hash"] != hash || stat["path"] != "render.py" {
		t.Fatalf("stat %d %v", code, stat)
	}

	code, list, _ := doJSON(t, "GET", base+"/v1/jobs/ops%2Freport/files?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, list)
	}
	listed, _ := list["files"].([]any)
	for _, item := range listed {
		m, _ := item.(map[string]any)
		p, _ := m["path"].(string)
		if strings.Contains(p, "other") || strings.HasPrefix(p, "report/") {
			t.Fatalf("job list leaked workspace layout: %v", list)
		}
	}

	code, _, _ = doRaw(t, "PUT", base+"/v1/jobs/ops%2Fother/files/secret.py?workspace="+wsID, nil, []byte("nope"))
	if code != 201 {
		t.Fatalf("sibling put %d", code)
	}
	code, list, _ = doJSON(t, "GET", base+"/v1/jobs/ops%2Freport/files?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("list after sibling %d %v", code, list)
	}
	listed, _ = list["files"].([]any)
	for _, item := range listed {
		m, _ := item.(map[string]any)
		if mapStrPath(m) == "secret.py" {
			t.Fatalf("sibling file visible in job list: %v", list)
		}
	}

	code, body412, _ := doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID,
		map[string]string{"If-Match": "deadbeef"}, []byte("v2"))
	if code != 412 || errCode(body412) != "precondition_failed" {
		t.Fatalf("stale %d %v", code, body412)
	}

	code, _, _ = doRaw(t, "DELETE", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil, nil)
	if code != 200 {
		t.Fatalf("delete %d", code)
	}
	code, _, _ = doRaw(t, "DELETE", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil, nil)
	if code != 404 {
		t.Fatalf("delete missing %d", code)
	}
}

func TestJobFilesErrors(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "job-files-err")
	createTestJob(t, base, wsID, "ops/report")

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/no-such/files/a.py", nil, []byte("x"))
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown job %d %v", code, body)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/%2e%2e%2fsibling%2fx?workspace="+wsID, nil, []byte("x"))
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("escape %d %v", code, body)
	}
	if msg := errMsg(body); strings.Contains(msg, wsDir) {
		t.Fatalf("echoed abs path: %s", msg)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/JOB.md?workspace="+wsID, nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("JOB.md put %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "trebi job") {
		t.Fatalf("JOB.md message: %s", errMsg(body))
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/tools/JOB.md?workspace="+wsID, nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("nested JOB.md %d %v", code, body)
	}

	code, body, _ = doRaw(t, "DELETE", base+"/v1/jobs/ops%2Freport/files/MEMORY.md?workspace="+wsID, nil, nil)
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("MEMORY.md delete %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "trebi job memory") {
		t.Fatalf("MEMORY.md message: %s", errMsg(body))
	}

	code, _, hdr := doRaw(t, "GET", base+"/v1/jobs/ops%2Freport/files/JOB.md?workspace="+wsID, nil, nil)
	if code != 200 {
		t.Fatalf("GET JOB.md %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/v1/jobs/ops%2Freport/files/JOB.md?workspace="+wsID, nil)
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if !strings.Contains(string(raw), "Do work") {
		t.Fatalf("GET JOB.md body %q hdr %v", raw, hdr)
	}
}

func TestJobFilesAmbiguous(t *testing.T) {
	_, base := startTestServer(t)
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	writeJobFile(t, ws1, "ops/dup")
	writeJobFile(t, ws2, "ops/dup")
	id1 := addWorkspace(t, base, ws1, "jf-a")
	_ = addWorkspace(t, base, ws2, "jf-b")

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/ops%2Fdup/files/a.py", nil, []byte("x"))
	if code != 400 || errCode(body) != "ambiguous" {
		t.Fatalf("want 400 ambiguous, got %d %v", code, body)
	}
	code, resp, _ := doRaw(t, "PUT", base+"/v1/jobs/ops%2Fdup/files/a.py?workspace="+id1, nil, []byte("x"))
	if code != 201 || resp["path"] != "a.py" {
		t.Fatalf("scoped put %d %v", code, resp)
	}
}

func TestJobFilesBusPathIsWorkspaceRelative(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "job-bus")
	createTestJob(t, base, wsID, "ops/report")

	code, _, _ := doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/render.py?workspace="+wsID, nil, []byte("v1"))
	if code != 201 {
		t.Fatalf("job put %d", code)
	}
	code, _, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/ops/report/render.py", nil, []byte("v2"))
	if code != 200 {
		t.Fatalf("ws put %d", code)
	}

	frames := collectBus(t, base, "file.created,file.updated", 600*time.Millisecond)
	wantID := wsID + ":agents/ops/report/render.py"
	var createdID, updatedID, createdPath, updatedPath string
	for _, f := range frames {
		id, _ := f["id"].(string)
		attrs, _ := f["attrs"].(map[string]any)
		p, _ := attrs["path"].(string)
		switch f["topic"] {
		case "file.created":
			createdID, createdPath = id, p
		case "file.updated":
			updatedID, updatedPath = id, p
		}
	}
	if createdID != wantID || updatedID != wantID {
		t.Fatalf("entity id created=%q updated=%q want %q frames=%v", createdID, updatedID, wantID, topicsOf(frames))
	}
	if createdPath != "agents/ops/report/render.py" || updatedPath != "agents/ops/report/render.py" {
		t.Fatalf("bus path created=%q updated=%q", createdPath, updatedPath)
	}
}

func TestJobFilesRootLevelJob(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "root-job")
	md := []byte("---\nharness: fake\n---\n\nRoot job.\n")
	code, _, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/JOB.md", nil, md)
	if code != 201 {
		t.Fatalf("root JOB.md %d", code)
	}
	jobName := url.PathEscape("/")
	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/"+jobName+"/files/trebi.yaml?workspace="+wsID, nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("trebi.yaml via root job %d %v", code, body)
	}
	code, resp, _ := doRaw(t, "PUT", base+"/v1/jobs/"+jobName+"/files/note.txt?workspace="+wsID, nil, []byte("hi"))
	if code != 201 || resp["path"] != "note.txt" {
		t.Fatalf("root-job put %d %v", code, resp)
	}
	onDisk, err := os.ReadFile(filepath.Join(wsDir, "note.txt"))
	if err != nil || string(onDisk) != "hi" {
		t.Fatalf("root-job disk: %q %v", onDisk, err)
	}
}

// TestJobFilesResolveAgentFolder: the job-scoped file jail also resolves
// kind=agent rows by index name (cmo), so the agent Files tab and
// `trebi job file agents/cmo` browse the agent folder. AGENT.md stays a
// typed 409 there; plain files under the agent folder write normally.
func TestJobFilesResolveAgentFolder(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-jail-files")

	code, list, _ := doJSON(t, "GET", base+"/v1/jobs/cmo/files?workspace="+wsID+"&recursive=1", nil)
	if code != 200 {
		t.Fatalf("agent file list %d %v", code, list)
	}
	paths := map[string]bool{}
	rows, _ := list["files"].([]any)
	for _, r := range rows {
		item, _ := r.(map[string]any)
		p, _ := item["path"].(string)
		paths[p] = true
	}
	if !paths["AGENT.md"] || !paths["duty"] {
		t.Fatalf("agent list missing AGENT.md or duty/: %v", paths)
	}

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/cmo/files/tools/run.py?workspace="+wsID, nil, []byte("print(1)\n"))
	if code != 201 || body["path"] != "tools/run.py" {
		t.Fatalf("agent file put %d %v", code, body)
	}
	onDisk, err := os.ReadFile(filepath.Join(wsDir, "agents", "cmo", "tools", "run.py"))
	if err != nil || string(onDisk) != "print(1)\n" {
		t.Fatalf("agent file on disk %q %v", onDisk, err)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/cmo/files/AGENT.md?workspace="+wsID, nil, []byte("sneak"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("agent AGENT.md put %d %v want 409 conflict", code, body)
	}
}

func TestJobFilesOversize(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{FilesMaxBytes: 8})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/report")
	wsID := addWorkspace(t, base, wsDir, "tiny-job")

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/ops%2Freport/files/big.bin?workspace="+wsID, nil, []byte("0123456789"))
	if code != 413 || errCode(body) != "validation" {
		t.Fatalf("oversize %d %v", code, body)
	}
}

// TestFilesTraversalNoRedirect pins the fix for a PUT whose path carries an
// unencoded ".." segment. http.ServeMux used to clean the path and answer 307
// before any handler ran, so the body was dropped and the caller saw success.
func TestFilesTraversalNoRedirect(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "files-traversal")

	paths := []string{
		"/v1/workspaces/" + wsID + "/files/../notes.txt",
		"/v1/workspaces/" + wsID + "/files/a/../../etc/passwd",
		"/v1/workspaces/" + wsID + "/files/./notes.txt",
		"/v1/workspaces/" + wsID + "/files/%2e%2e/notes.txt",
	}
	for _, p := range paths {
		code, body, _ := doRaw(t, "PUT", base+p, nil, []byte("SHOULD NOT LAND"))
		if code != 400 || errCode(body) != "validation" {
			t.Errorf("PUT %s: got %d %v, want 400 validation", p, code, body)
		}
	}
	// Nothing escaped the jail, and no stray file landed in the workspace.
	if _, err := os.Stat(filepath.Join(filepath.Dir(wsDir), "notes.txt")); err == nil {
		t.Fatal("wrote outside the workspace root")
	}
	if _, err := os.Stat(filepath.Join(wsDir, "notes.txt")); err == nil {
		t.Fatal("wrote notes.txt into the workspace")
	}
	// A normal path still works.
	code, body, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/ok.txt", nil, []byte("v1"))
	if code != 201 {
		t.Fatalf("plain put %d %v", code, body)
	}
}

// createTestJob creates ops/<name>, and agent ops when it is missing.
func createTestJob(t *testing.T, base, wsID, name string) {
	t.Helper()
	ensureAgent(t, base, wsID, "ops")
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": name, "agent": "ops", "markdown": "---\nharness: fake\n---\n\nDo work.\n",
	})
	if code != 201 {
		t.Fatalf("create job %s: %d %v", name, code, body)
	}
}

func mapStrPath(m map[string]any) string {
	p, _ := m["path"].(string)
	return p
}

func doRaw(t *testing.T, method, url string, headers map[string]string, body []byte) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != nil && method != "GET" && method != "DELETE" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, resp.Header
}

func errMsg(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	return fmt.Sprint(e["message"])
}

func collectBus(t *testing.T, base, topics string, d time.Duration) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/stream?cursor=0&topics="+topics, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var frames []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m) == nil {
			frames = append(frames, m)
		}
	}
	return frames
}

func hasTopic(frames []map[string]any, topic string) bool {
	for _, f := range frames {
		if f["topic"] == topic {
			return true
		}
	}
	return false
}

func topicsOf(frames []map[string]any) []string {
	var out []string
	for _, f := range frames {
		if t, ok := f["topic"].(string); ok {
			out = append(out, t)
		}
	}
	return out
}

func TestFolderRoutes(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "folders")
	createTestJob(t, base, wsID, "ops/report")
	if code, sk, _ := doJSON(t, "POST", base+"/v1/skills", map[string]any{"name": "bundle", "skill_md": "# Bundle\n\nBody.\n"}); code != 201 {
		t.Fatalf("create skill %d %v", code, sk)
	}
	owners := []struct {
		name, prefix, query, doc string
	}{
		{"workspace", "/v1/workspaces/" + wsID, "", ""},
		{"job", "/v1/jobs/ops%2Freport", "?workspace=" + wsID, "JOB.md"},
		{"skill", "/v1/skills/bundle", "", "SKILL.md"},
	}
	for _, o := range owners {
		t.Run(o.name, func(t *testing.T) {
			url := func(p string, extra ...string) string {
				q := o.query
				for _, e := range extra {
					if q == "" {
						q = "?" + e
					} else {
						q += "&" + e
					}
				}
				return base + o.prefix + p + q
			}
			code, body, _ := doJSON(t, "POST", url("/folders/out/empty"), nil)
			if code != 201 || body["path"] != "out/empty" || body["dir"] != true {
				t.Fatalf("mkdir %d %v", code, body)
			}
			if code, body, _ = doJSON(t, "POST", url("/folders/out/empty"), nil); code != 409 || errCode(body) != "conflict" {
				t.Fatalf("mkdir again %d %v", code, body)
			}
			for _, f := range []string{"out/a.txt", "b.txt"} {
				if code, body, _ := doRaw(t, "PUT", url("/files/"+f), nil, []byte(f)); code != 201 {
					t.Fatalf("put %s %d %v", f, code, body)
				}
			}
			code, body, _ = doJSON(t, "POST", url("/files/move"), map[string]any{"from": "out/a.txt", "to": "arch/a.txt"})
			if code != 200 || body["path"] != "arch/a.txt" || body["hash"] == "" {
				t.Fatalf("move %d %v", code, body)
			}
			if code, body, _ = doJSON(t, "POST", url("/files/move"), map[string]any{"from": "b.txt", "to": "arch/a.txt"}); code != 409 {
				t.Fatalf("move onto file %d %v", code, body)
			}
			if code, body, _ = doJSONHeaders(t, "POST", url("/files/move"), map[string]any{"from": "b.txt", "to": "arch/a.txt", "overwrite": true}, map[string]string{"If-Match": "stale"}); code != 412 {
				t.Fatalf("stale move %d %v", code, body)
			}
			if code, body, _ = doJSON(t, "POST", url("/files/move"), map[string]any{"from": "b.txt", "to": "arch/a.txt", "overwrite": true}); code != 200 {
				t.Fatalf("overwrite move %d %v", code, body)
			}
			if code, body, _ = doJSON(t, "POST", url("/files/move"), map[string]any{"from": "arch", "to": "arch/in"}); code != 400 {
				t.Fatalf("into itself %d %v", code, body)
			}
			if code, body, _ = doJSON(t, "POST", url("/files/move"), map[string]any{"from": "out", "to": "moved/out"}); code != 200 || body["dir"] != true {
				t.Fatalf("folder move %d %v", code, body)
			}
			if o.doc != "" {
				if code, body, _ = doJSON(t, "POST", url("/files/move"), map[string]any{"from": o.doc, "to": "x.md"}); code != 409 {
					t.Fatalf("move %s %d %v", o.doc, code, body)
				}
			}
			code, body, _ = doJSON(t, "DELETE", url("/folders/arch"), nil)
			if code != 400 || !strings.Contains(errMsg(body), "the 1 files in it") {
				t.Fatalf("rmdir without confirm %d %v", code, body)
			}
			code, body, _ = doJSON(t, "DELETE", url("/folders/arch", "confirm=1"), nil)
			if code != 200 || body["files_removed"] != float64(1) {
				t.Fatalf("rmdir %d %v", code, body)
			}
			if code, body, _ = doJSON(t, "DELETE", url("/folders/arch", "confirm=1"), nil); code != 404 {
				t.Fatalf("rmdir missing %d %v", code, body)
			}
			code, list, _ := doJSON(t, "GET", url("/files", "recursive=1"), nil)
			if code != 200 || !strings.Contains(fmt.Sprint(list["files"]), "moved/out/empty") {
				t.Fatalf("empty folder not listed %d %v", code, list)
			}
		})
	}

	// A folder delete that holds JOB.md removes the job from the index.
	if code, body, _ := doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/folders/agents/ops/report?confirm=1", nil); code != 200 {
		t.Fatalf("rmdir job folder %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "GET", base+"/v1/jobs/ops%2Freport?workspace="+wsID, nil); code != 404 {
		t.Fatalf("job after folder delete %d %v", code, body)
	}
}

func TestFolderRoutesOwnedJob(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "owned-folders")
	prefix := base + "/v1/jobs/cmo/duty"
	q := "?workspace=" + wsID
	if code, body, _ := doJSON(t, "POST", prefix+"/folders/tmp"+q, nil); code != 201 || body["path"] != "tmp" {
		t.Fatalf("mkdir %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", prefix+"/files/move"+q, map[string]any{"from": "tmp", "to": "kept"}); code != 200 || body["path"] != "kept" {
		t.Fatalf("move %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "DELETE", prefix+"/folders/kept"+q, nil); code != 400 || errDetailsOf(body)["confirm_required"] != true {
		t.Fatalf("rmdir without confirm %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "DELETE", prefix+"/folders/kept"+q+"&confirm=1", nil); code != 200 {
		t.Fatalf("rmdir %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "duty", "kept")); !os.IsNotExist(err) {
		t.Fatalf("folder still there: %v", err)
	}
}

func errDetailsOf(m map[string]any) map[string]any {
	e, _ := m["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

// ensureAgent creates the agent through the API when it is missing.
func ensureAgent(t *testing.T, base, wsID, name string) {
	t.Helper()
	if code, _, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/"+name, nil); code != http.StatusNotFound {
		return
	}
	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/"+name, map[string]any{
		"content": "---\ndescription: Test agent.\nharness: fake\n---\n\nRun.\n",
	})
	if code != 201 && code != 200 {
		t.Fatalf("create agent %s: %d %v", name, code, body)
	}
}
