//go:build live

package runs_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// Live restart (scripts/test.resume.sh --restart): a real daemon stops in the
// middle of a turn. After boot, the run must continue by itself in the same
// session and still know the code word of turn 1.
//
// Env: as TestLiveResume, plus TREBI_LIVE_SIGNALS (default term,kill).
func TestLiveRestart(t *testing.T) {
	names := strings.Split(cmp.Or(os.Getenv("TREBI_LIVE_HARNESSES"), "claude,codex,pi,cursor,opencode,copilot,gemini"), ",")
	signals := strings.Split(cmp.Or(os.Getenv("TREBI_LIVE_SIGNALS"), "term,kill"), ",")
	out := cmp.Or(os.Getenv("TREBI_LIVE_OUT"), filepath.Join("..", "..", "build", "live-restart.txt"))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "trebi")
	if b, err := exec.Command("go", "build", "-o", bin, "../../cmd/trebi").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	for _, name := range names {
		for _, sig := range signals {
			name, sig := strings.TrimSpace(name), strings.TrimSpace(sig)
			t.Run(name+"/"+sig, func(t *testing.T) {
				result := "FAIL"
				t.Cleanup(func() {
					if t.Skipped() {
						result = "SKIP"
					} else if !t.Failed() {
						result = "PASS"
					}
					line := fmt.Sprintf("%s %s %s %s\n", time.Now().UTC().Format(time.RFC3339), name, sig, result)
					if f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
						_, _ = f.WriteString(line)
						_ = f.Close()
					}
				})
				liveRestart(t, bin, name, sig)
			})
		}
	}
}

func liveRestart(t *testing.T, bin, name, sig string) {
	tool, ok := agentcli.ByName(name)
	if !ok {
		t.Fatalf("unknown harness %s", name)
	}
	vendor := name
	if name == agentcli.Cursor {
		vendor = "cursor-agent"
	}
	path, err := exec.LookPath(vendor)
	if err != nil {
		t.Skipf("%s not on PATH", vendor)
	}
	if st := tool.ProbeAuth(t.Context()); st.Status != "ok" && st.Status != "unknown" {
		if os.Getenv("TREBI_LIVE_SKIP_NOAUTH") == "1" {
			t.Skipf("%s auth %s: %s", name, st.Status, st.Hint)
		}
		t.Fatalf("%s auth %s: %s", name, st.Status, st.Hint)
	}

	d := &liveDaemon{t: t, bin: bin, home: t.TempDir()}
	cfg := "tools:\n  " + name + ":\n    enabled: true\n    path: " + path + "\n"
	// A separate login (cursor) lives in the harness folder of the installed home.
	if seed, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".trebi", "harness", name, "cli-config.json")); err == nil {
		cfg += "    auth: separate\n"
		dir := filepath.Join(d.home, "harness", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cli-config.json"), seed, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start()
	t.Cleanup(d.stop)

	ws := t.TempDir()
	model := cmp.Or(os.Getenv("TREBI_LIVE_MODEL_"+strings.ToUpper(name)), liveResumeModels[name])
	front := "harness:\n  name: " + name + "\n"
	if model != "" {
		front += "  model: " + model + "\n"
	}
	if err := os.MkdirAll(filepath.Join(ws, "count"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "count", "JOB.md"), []byte("---\n"+front+"---\n\nDo the task.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var wsResp struct{ ID string }
	d.do("POST", "/v1/workspaces", map[string]any{"path": ws, "name": "live"}, &wsResp)

	word := liveWord(t)
	prompt := "Remember the code word " + word + ". First run the shell command `sleep 20`. Then write the numbers from 1 to 20 in English words, one number on each line. After the last number, write the code word on its own line."
	var run runs.Run
	d.do("POST", "/v1/jobs/count/run", map[string]any{"workspace": wsResp.ID, "prompt_override": prompt}, &run)

	// Stop the daemon in the middle of the turn: when the agent has started
	// to answer, or 8 s after the session started.
	deadline := time.Now().Add(3 * time.Minute)
	var started time.Time
	for {
		if time.Now().After(deadline) {
			t.Fatal("no reply in 3m")
		}
		cur := d.run(run.ID)
		if cur.Status != runs.StatusRunning {
			t.Fatalf("turn 1 ended before the restart: %s/%s", cur.Status, cur.EndReason)
		}
		if started.IsZero() && cur.SessionID != "" {
			started = time.Now()
		}
		if d.replied(run.ID) || (!started.IsZero() && time.Since(started) > 8*time.Second) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if sig == "kill" {
		d.signal(syscall.SIGKILL)
	} else {
		d.signal(syscall.SIGTERM)
		paused := d.runOffline(run.ID)
		if paused.Status != runs.StatusParked || paused.EndReason != runs.EndRestart {
			t.Fatalf("after SIGTERM: %s/%s, want parked/restart", paused.Status, paused.EndReason)
		}
	}
	d.start()

	var end *runs.Run
	deadline = time.Now().Add(5 * time.Minute)
	for end == nil {
		if time.Now().After(deadline) {
			t.Fatal("turn 2 did not end in 5m")
		}
		cur := d.run(run.ID)
		if cur.TurnCount >= 2 && cur.Status.IsTerminal() && cur.Status != runs.StatusParked {
			end = cur
		}
		time.Sleep(time.Second)
	}
	if end.Status != runs.StatusComplete || end.TurnCount != 2 {
		t.Fatalf("turn 2 ended %s/%s turns %d, want completed and 2", end.Status, end.EndReason, end.TurnCount)
	}
	resumed, reply := false, ""
	for _, e := range d.transcript(run.ID) {
		if e.RunTurn != 2 {
			continue
		}
		switch e.Role {
		case "user":
			resumed = resumed || strings.Contains(e.Text, "Trebi restarted")
		case "assistant":
			reply = e.Text
		}
	}
	if !resumed {
		t.Error("turn 2 has no restart prompt")
	}
	if !strings.Contains(strings.ToLower(reply), word) {
		t.Fatalf("turn 2 reply lacks the code word %q: %q", word, reply)
	}
}

// liveDaemon is one trebi daemon process on a free port with its own home.
type liveDaemon struct {
	t    *testing.T
	bin  string
	home string
	addr string
	cmd  *exec.Cmd
	log  *os.File
}

func (d *liveDaemon) start() {
	d.t.Helper()
	if d.addr == "" {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			d.t.Fatal(err)
		}
		d.addr = l.Addr().String()
		_ = l.Close()
	}
	f, err := os.OpenFile(filepath.Join(d.home, "daemon.out"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		d.t.Fatal(err)
	}
	d.log = f
	d.cmd = exec.Command(d.bin, "service", "daemon", "--listen", d.addr, "--log-level", "debug")
	d.cmd.Env = append(os.Environ(), "TREBI_HOME="+d.home)
	d.cmd.Stdout, d.cmd.Stderr = f, f
	if err := d.cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	for range 100 {
		if resp, err := http.Get("http://" + d.addr + "/v1/system/health"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	b, _ := os.ReadFile(f.Name())
	d.t.Fatalf("daemon did not start:\n%s", tail(string(b), 40))
}

func (d *liveDaemon) signal(sig syscall.Signal) {
	d.t.Helper()
	if err := d.cmd.Process.Signal(sig); err != nil {
		d.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = d.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Minute):
		d.t.Fatalf("daemon did not exit on %s", sig)
	}
	_ = d.log.Close()
	d.cmd = nil
}

func (d *liveDaemon) stop() {
	if d.cmd != nil {
		d.signal(syscall.SIGTERM)
	}
	if !d.t.Failed() {
		return
	}
	if b, err := os.ReadFile(filepath.Join(d.home, "daemon.out")); err == nil {
		d.t.Logf("daemon log tail:\n%s", tail(string(b), 40))
	}
	st, err := store.OpenPath(filepath.Join(d.home, "trebi.db"))
	if err != nil {
		return
	}
	defer st.Close()
	rows, err := st.DB().QueryContext(context.Background(), `SELECT run_id, kind, COALESCE(detail_json,'') FROM run_events WHERE kind IN ('error','failed','launch_failed') OR detail_json LIKE '%error%' ORDER BY at`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, detail string
		if rows.Scan(&id, &kind, &detail) == nil {
			d.t.Logf("run event %s %s %s", id, kind, detail)
		}
	}
}

func (d *liveDaemon) do(method, path string, body, out any) {
	d.t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(d.t.Context(), method, "http://"+d.addr+path, r)
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 {
		d.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw.String())
	}
	if out != nil {
		if err := json.Unmarshal(raw.Bytes(), out); err != nil {
			d.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

func (d *liveDaemon) run(id string) *runs.Run {
	var r runs.Run
	d.do("GET", "/v1/runs/"+id, nil, &r)
	return &r
}

// runOffline reads the run from the store while the daemon is down.
func (d *liveDaemon) runOffline(id string) *runs.Run {
	d.t.Helper()
	st, err := store.OpenPath(filepath.Join(d.home, "trebi.db"))
	if err != nil {
		d.t.Fatal(err)
	}
	defer st.Close()
	run, err := runs.New(st, nil, files.New(files.DefaultMaxBytes)).Get(d.t.Context(), id)
	if err != nil {
		d.t.Fatal(err)
	}
	return run
}

func (d *liveDaemon) transcript(id string) []agentcli.Turn {
	var tr struct{ Turns []agentcli.Turn }
	d.do("GET", "/v1/runs/"+id+"/transcript", nil, &tr)
	return tr.Turns
}

// replied reports a tool call or an assistant entry in the transcript.
func (d *liveDaemon) replied(id string) bool {
	req, _ := http.NewRequestWithContext(d.t.Context(), "GET", "http://"+d.addr+"/v1/runs/"+id+"/transcript", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var tr struct{ Turns []agentcli.Turn }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&tr) != nil {
		return false
	}
	for _, e := range tr.Turns {
		if e.Role == "tools" || (e.Role == "assistant" && e.Text != "") {
			return true
		}
	}
	return false
}

// tail returns the last n log lines, without the request lines.
func tail(s string, n int) string {
	var lines []string
	for l := range strings.SplitSeq(s, "\n") {
		if !strings.Contains(l, "msg=api.request") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
