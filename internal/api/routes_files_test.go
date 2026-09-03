package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "files-ws")

	body := []byte("print('hi')\n")
	code, resp, hdr := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/reports/render.py", s.Token(), nil, body)
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

	code, resp, _ = doRaw(t, "GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?confirm=1", s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, resp)
	}

	req, _ := http.NewRequest("GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
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
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("If-None-Match", `"`+hash+`"`)
	httpResp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != 304 {
		t.Fatalf("if-none-match %d", httpResp.StatusCode)
	}

	code, stat, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?stat=1", s.Token(), nil)
	if code != 200 || stat["hash"] != hash {
		t.Fatalf("stat %d %v", code, stat)
	}

	code, list, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files?prefix=reports/", s.Token(), nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, list)
	}
	files, _ := list["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("list: %v", list)
	}

	code, _, _ = doRaw(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?confirm=1", s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("delete %d", code)
	}
	code, _, _ = doRaw(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/reports/render.py?confirm=1", s.Token(), nil, nil)
	if code != 404 {
		t.Fatalf("delete missing %d", code)
	}
}

func TestFilesErrors(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "files-err")

	code, body, _ := doRaw(t, "PUT", base+"/v1/workspaces/no-such/files/a.py", s.Token(), nil, []byte("x"))
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("missing ws %d %v", code, body)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/%2e%2e%2f%2e%2e%2fx", s.Token(), nil, []byte("x"))
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("escape %d %v", code, body)
	}
	if msg := errMsg(body); strings.Contains(msg, wsDir) {
		t.Fatalf("echoed abs path: %s", msg)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/trebi.yaml", s.Token(), nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("trebi.yaml %d %v", code, body)
	}

	code, created, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/f.txt", s.Token(), nil, []byte("v1"))
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/f.txt", s.Token(),
		map[string]string{"If-Match": "deadbeef"}, []byte("v2"))
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("stale %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "current hash:") {
		t.Fatalf("412 message: %s", errMsg(body))
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/missing.txt", s.Token(),
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
	s := api.New(st, api.Options{Token: "test-token", FilesMaxBytes: 8})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "tiny")

	code, body, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/big.bin", s.Token(), nil, []byte("0123456789"))
	if code != 413 || errCode(body) != "validation" {
		t.Fatalf("oversize %d %v", code, body)
	}
}

func TestFilesJobMarkdownHook(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "files-job")
	md := []byte("---\nharness: fake\n---\n\nDo work.\n")

	code, created, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/reports/JOB.md", s.Token(), nil, md)
	if code != 201 {
		t.Fatalf("put job %d %v", code, created)
	}
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/reports?workspace="+wsID, s.Token(), nil)
	if code != 200 {
		t.Fatalf("get job %d %v", code, got)
	}
	if got["hash"] != job.ContentHash(md) {
		t.Fatalf("hash mismatch job=%v file=%s", got["hash"], job.ContentHash(md))
	}

	frames := collectBus(t, base, s.Token(), "file.created,file.deleted,job.created,job.deleted", 600*time.Millisecond)
	if !hasTopic(frames, "file.created") || !hasTopic(frames, "job.created") {
		t.Fatalf("bus after create: %v", topicsOf(frames))
	}

	code, _, _ = doRaw(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/reports/JOB.md?confirm=1", s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("delete job %d", code)
	}
	frames = collectBus(t, base, s.Token(), "file.deleted,job.deleted", 600*time.Millisecond)
	if !hasTopic(frames, "file.deleted") || !hasTopic(frames, "job.deleted") {
		t.Fatalf("bus after delete: %v", topicsOf(frames))
	}
}

func TestFilesIdenticalJobWriteNoDupUpdate(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "files-dup")
	md := []byte("---\nharness: fake\n---\n\nSame.\n")
	url := base + "/v1/workspaces/" + wsID + "/files/dup/JOB.md"
	if code, _, _ := doRaw(t, "PUT", url, s.Token(), nil, md); code != 201 {
		t.Fatalf("first %d", code)
	}
	if code, _, _ := doRaw(t, "PUT", url, s.Token(), nil, md); code != 200 {
		t.Fatalf("second %d", code)
	}
	frames := collectBus(t, base, s.Token(), "job.created,job.updated", 600*time.Millisecond)
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
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "audit-ws")
	code, _, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/a.py", s.Token(), nil, []byte("x"))
	if code != 201 {
		t.Fatalf("put %d (audit wrapper must not block raw PUT)", code)
	}
}

func TestJobFilesCRUD(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "job-files")
	createTestJob(t, base, s.Token(), wsID, "report")
	createTestJob(t, base, s.Token(), wsID, "other")

	body := []byte("print('hi')\n")
	code, resp, hdr := doRaw(t, "PUT", base+"/v1/jobs/report/files/render.py?workspace="+wsID, s.Token(), nil, body)
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
	onDisk, err := os.ReadFile(filepath.Join(wsDir, "report", "render.py"))
	if err != nil || string(onDisk) != string(body) {
		t.Fatalf("disk: %q %v", onDisk, err)
	}

	code, _, _ = doRaw(t, "GET", base+"/v1/jobs/report/files/render.py?workspace="+wsID, s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("get %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/v1/jobs/report/files/render.py?workspace="+wsID, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if string(raw) != string(body) {
		t.Fatalf("body %q", raw)
	}

	req, _ = http.NewRequest("GET", base+"/v1/jobs/report/files/render.py?workspace="+wsID, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("If-None-Match", `"`+hash+`"`)
	httpResp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != 304 {
		t.Fatalf("if-none-match %d", httpResp.StatusCode)
	}

	code, stat, _ := doJSON(t, "GET", base+"/v1/jobs/report/files/render.py?stat=1&workspace="+wsID, s.Token(), nil)
	if code != 200 || stat["hash"] != hash || stat["path"] != "render.py" {
		t.Fatalf("stat %d %v", code, stat)
	}

	code, list, _ := doJSON(t, "GET", base+"/v1/jobs/report/files?workspace="+wsID, s.Token(), nil)
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

	code, _, _ = doRaw(t, "PUT", base+"/v1/jobs/other/files/secret.py?workspace="+wsID, s.Token(), nil, []byte("nope"))
	if code != 201 {
		t.Fatalf("sibling put %d", code)
	}
	code, list, _ = doJSON(t, "GET", base+"/v1/jobs/report/files?workspace="+wsID, s.Token(), nil)
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

	code, body412, _ := doRaw(t, "PUT", base+"/v1/jobs/report/files/render.py?workspace="+wsID, s.Token(),
		map[string]string{"If-Match": "deadbeef"}, []byte("v2"))
	if code != 412 || errCode(body412) != "precondition_failed" {
		t.Fatalf("stale %d %v", code, body412)
	}

	code, _, _ = doRaw(t, "DELETE", base+"/v1/jobs/report/files/render.py?workspace="+wsID, s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("delete %d", code)
	}
	code, _, _ = doRaw(t, "DELETE", base+"/v1/jobs/report/files/render.py?workspace="+wsID, s.Token(), nil, nil)
	if code != 404 {
		t.Fatalf("delete missing %d", code)
	}
}

func TestJobFilesErrors(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "job-files-err")
	createTestJob(t, base, s.Token(), wsID, "report")

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/no-such/files/a.py", s.Token(), nil, []byte("x"))
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown job %d %v", code, body)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/report/files/%2e%2e%2fsibling%2fx?workspace="+wsID, s.Token(), nil, []byte("x"))
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("escape %d %v", code, body)
	}
	if msg := errMsg(body); strings.Contains(msg, wsDir) {
		t.Fatalf("echoed abs path: %s", msg)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/report/files/JOB.md?workspace="+wsID, s.Token(), nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("JOB.md put %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "trebi job") {
		t.Fatalf("JOB.md message: %s", errMsg(body))
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/jobs/report/files/tools/JOB.md?workspace="+wsID, s.Token(), nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("nested JOB.md %d %v", code, body)
	}

	code, body, _ = doRaw(t, "DELETE", base+"/v1/jobs/report/files/MEMORY.md?workspace="+wsID, s.Token(), nil, nil)
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("MEMORY.md delete %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "trebi job memory") {
		t.Fatalf("MEMORY.md message: %s", errMsg(body))
	}

	code, _, hdr := doRaw(t, "GET", base+"/v1/jobs/report/files/JOB.md?workspace="+wsID, s.Token(), nil, nil)
	if code != 200 {
		t.Fatalf("GET JOB.md %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/v1/jobs/report/files/JOB.md?workspace="+wsID, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
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
	s, base := startTestServer(t)
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	writeJobFile(t, ws1, "dup")
	writeJobFile(t, ws2, "dup")
	id1 := addWorkspace(t, base, s.Token(), ws1, "jf-a")
	_ = addWorkspace(t, base, s.Token(), ws2, "jf-b")

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/dup/files/a.py", s.Token(), nil, []byte("x"))
	if code != 400 || errCode(body) != "ambiguous" {
		t.Fatalf("want 400 ambiguous, got %d %v", code, body)
	}
	code, resp, _ := doRaw(t, "PUT", base+"/v1/jobs/dup/files/a.py?workspace="+id1, s.Token(), nil, []byte("x"))
	if code != 201 || resp["path"] != "a.py" {
		t.Fatalf("scoped put %d %v", code, resp)
	}
}

func TestJobFilesBusPathIsWorkspaceRelative(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "job-bus")
	createTestJob(t, base, s.Token(), wsID, "report")

	code, _, _ := doRaw(t, "PUT", base+"/v1/jobs/report/files/render.py?workspace="+wsID, s.Token(), nil, []byte("v1"))
	if code != 201 {
		t.Fatalf("job put %d", code)
	}
	code, _, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/report/render.py", s.Token(), nil, []byte("v2"))
	if code != 200 {
		t.Fatalf("ws put %d", code)
	}

	frames := collectBus(t, base, s.Token(), "file.created,file.updated", 600*time.Millisecond)
	wantID := wsID + ":report/render.py"
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
	if createdPath != "report/render.py" || updatedPath != "report/render.py" {
		t.Fatalf("bus path created=%q updated=%q", createdPath, updatedPath)
	}
}

func TestJobFilesRootLevelJob(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "root-job")
	md := []byte("---\nharness: fake\n---\n\nRoot job.\n")
	code, _, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/JOB.md", s.Token(), nil, md)
	if code != 201 {
		t.Fatalf("root JOB.md %d", code)
	}
	jobName := filepath.Base(wsDir)
	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/"+jobName+"/files/trebi.yaml?workspace="+wsID, s.Token(), nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("trebi.yaml via root job %d %v", code, body)
	}
	code, resp, _ := doRaw(t, "PUT", base+"/v1/jobs/"+jobName+"/files/note.txt?workspace="+wsID, s.Token(), nil, []byte("hi"))
	if code != 201 || resp["path"] != "note.txt" {
		t.Fatalf("root-job put %d %v", code, resp)
	}
	onDisk, err := os.ReadFile(filepath.Join(wsDir, "note.txt"))
	if err != nil || string(onDisk) != "hi" {
		t.Fatalf("root-job disk: %q %v", onDisk, err)
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
	s := api.New(st, api.Options{Token: "test-token", FilesMaxBytes: 8})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "report")
	wsID := addWorkspace(t, base, s.Token(), wsDir, "tiny-job")

	code, body, _ := doRaw(t, "PUT", base+"/v1/jobs/report/files/big.bin?workspace="+wsID, s.Token(), nil, []byte("0123456789"))
	if code != 413 || errCode(body) != "validation" {
		t.Fatalf("oversize %d %v", code, body)
	}
}

// TestFilesTraversalNoRedirect pins the fix for a PUT whose path carries an
// unencoded ".." segment. http.ServeMux used to clean the path and answer 307
// before any handler ran, so the body was dropped and the caller saw success.
func TestFilesTraversalNoRedirect(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "files-traversal")

	paths := []string{
		"/v1/workspaces/" + wsID + "/files/../notes.txt",
		"/v1/workspaces/" + wsID + "/files/a/../../etc/passwd",
		"/v1/workspaces/" + wsID + "/files/./notes.txt",
		"/v1/workspaces/" + wsID + "/files/%2e%2e/notes.txt",
	}
	for _, p := range paths {
		code, body, _ := doRaw(t, "PUT", base+p, s.Token(), nil, []byte("SHOULD NOT LAND"))
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
	code, body, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/ok.txt", s.Token(), nil, []byte("v1"))
	if code != 201 {
		t.Fatalf("plain put %d %v", code, body)
	}
}

func createTestJob(t *testing.T, base, token, wsID, name string) {
	t.Helper()
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", token, map[string]any{
		"workspace": wsID, "name": name, "markdown": "---\nharness: fake\n---\n\nDo work.\n",
	})
	if code != 201 {
		t.Fatalf("create job %s: %d %v", name, code, body)
	}
}

func mapStrPath(m map[string]any) string {
	p, _ := m["path"].(string)
	return p
}

func doRaw(t *testing.T, method, url, token string, headers map[string]string, body []byte) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+token)
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

func collectBus(t *testing.T, base, token, topics string, d time.Duration) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/stream?cursor=0&topics="+topics, nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
