package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Outcome classifies a finished trial.
type Outcome string

const (
	OutcomePass         Outcome = "pass"
	OutcomeFail         Outcome = "fail"
	OutcomeInfraFailure Outcome = "infra-failure"
)

// CheckResult is one deterministic grader outcome.
type CheckResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
	Infra  bool   `json:"infra,omitempty"`
}

// JudgeResult is one dimension verdict.
type JudgeResult struct {
	Name     string  `json:"name"`
	Score    float64 `json:"score"`
	Min      float64 `json:"min"`
	Verdict  string  `json:"verdict"`
	Evidence string  `json:"evidence,omitempty"`
	Unknown  bool    `json:"unknown"`
	Pass     bool    `json:"pass"`
}

// TrialResult is the full record of one isolated trial.
type TrialResult struct {
	TaskID    string        `json:"task_id"`
	Client    string        `json:"client"`
	Index     int           `json:"index"`
	Outcome   Outcome       `json:"outcome"`
	Checks    []CheckResult `json:"checks"`
	Judge     []JudgeResult `json:"judge,omitempty"`
	CostUSD   float64       `json:"cost_usd"`
	DurationS float64       `json:"duration_s"`
	Error     string        `json:"error,omitempty"`
	Dir       string        `json:"dir"`
}

// Passed reports deterministic checks and judge minima all green.
func (r *TrialResult) Passed() bool {
	return r != nil && r.Outcome == OutcomePass
}

// trialEnv is one isolated TREBI_HOME + workdir + daemon child.
type trialEnv struct {
	home    string
	workdir string
	artDir  string
	apiURL  string
	bin     string
	cmd     *exec.Cmd
	parentW *os.File
	hashes  map[string]string
}

func newTrialEnv(home, workdir, artDir, bin string) *trialEnv {
	home, _ = filepath.Abs(home)
	workdir, _ = filepath.Abs(workdir)
	artDir, _ = filepath.Abs(artDir)
	bin, _ = filepath.Abs(bin)
	return &trialEnv{
		home:    home,
		workdir: workdir,
		artDir:  artDir,
		bin:     bin,
	}
}

func (e *trialEnv) placeholders(workspace string) Placeholders {
	return Placeholders{
		Home:       e.home,
		Workspace:  workspace,
		APIURL:     e.apiURL,
		Workdir:    e.workdir,
		Transcript: filepath.Join(e.artDir, "transcript.json"),
		Snapshot:   filepath.Join(e.artDir, "snapshot.json"),
	}
}

func (e *trialEnv) environ() []string {
	env := os.Environ()
	env = filterEnv(env, "TREBI_HOME", "TREBI_URL", "TREBI_TOKEN", "TREBI_WORKSPACE")
	env = append(env,
		"TREBI_HOME="+e.home,
		"PATH="+filepath.Dir(e.bin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return env
}

func (e *trialEnv) startDaemon(ctx context.Context, harness, wsRoot string) error {
	if err := os.MkdirAll(e.home, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(e.workdir, 0o755); err != nil {
		return err
	}
	if err := e.writeBootConfig(harness, wsRoot); err != nil {
		return err
	}
	logPath := filepath.Join(e.home, "logs", "trebid.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	_ = os.Symlink(logPath, filepath.Join(e.artDir, "daemon.log"))
	r, w, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		return err
	}
	cmd := exec.Command(e.bin, "service", "daemon",
		"--listen", "127.0.0.1:0", "--no-tray",
		"--parent-pid", strconv.Itoa(os.Getpid()))
	cmd.Env = append(e.environ(), "TREBI_PARENT_FD=3")
	cmd.Dir = e.workdir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{r}
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		_ = logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	_ = r.Close()
	e.cmd = cmd
	e.parentW = w
	if err := e.waitAPIJSON(ctx, 20*time.Second); err != nil {
		e.kill()
		return err
	}
	return nil
}

func (e *trialEnv) waitAPIJSON(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	path := filepath.Join(e.home, "api.json")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s", path)
		}
		data, err := os.ReadFile(path)
		if err == nil {
			var doc struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(data, &doc) == nil && doc.URL != "" {
				e.apiURL = doc.URL
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *trialEnv) stop(ctx context.Context) {
	e.closeParentPipe()
	e.killTrialTmux()
	if b, err := os.ReadFile(filepath.Join(e.workdir, "remote.pid")); err == nil {
		var pid int
		if _, err := fmt.Sscanf(string(b), "%d", &pid); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	if e.cmd == nil || e.cmd.Process == nil {
		e.killPIDFile()
		return
	}
	shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = e.run(shutCtx, e.bin, "service", "shutdown")
	done := make(chan struct{})
	go func() {
		_ = e.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		e.kill()
		<-done
	}
	e.killPIDFile()
}

func (e *trialEnv) closeParentPipe() {
	if e.parentW == nil {
		return
	}
	_ = e.parentW.Close()
	e.parentW = nil
}

func (e *trialEnv) kill() {
	e.closeParentPipe()
	killProcGroup(e.cmd)
	e.killPIDFile()
}

func (e *trialEnv) killPIDFile() {
	b, err := os.ReadFile(filepath.Join(e.home, "trebid.pid"))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil || pid <= 0 {
		return
	}
	if e.cmd != nil && e.cmd.Process != nil && e.cmd.Process.Pid == pid {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func (e *trialEnv) killTrialTmux() {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{session_name}\t#{pane_current_path}").Output()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		sess, path, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || sess == "" {
			continue
		}
		if !strings.HasPrefix(path, e.workdir) && !strings.HasPrefix(path, e.home) {
			continue
		}
		if seen[sess] {
			continue
		}
		seen[sess] = true
		_ = exec.Command("tmux", "kill-session", "-t", sess).Run()
	}
}

func (e *trialEnv) run(ctx context.Context, name string, args ...string) error {
	_, _, err := e.runOut(ctx, name, args...)
	return err
}

func (e *trialEnv) runOut(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.workdir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

func (e *trialEnv) trebi(ctx context.Context, args ...string) (stdout []byte, err error) {
	out, errb, err := e.runOut(ctx, e.bin, args...)
	if err != nil {
		return out, fmt.Errorf("trebi %s: %w\n%s", strings.Join(args, " "), err, errb)
	}
	return out, nil
}

func (e *trialEnv) seed(ctx context.Context, script string, p Placeholders) error {
	if script == "" {
		return nil
	}
	data, err := os.ReadFile(script)
	if err != nil {
		return fmt.Errorf("read seed: %w", err)
	}
	expanded := p.Expand(string(data))
	path := filepath.Join(e.workdir, "_seed.sh")
	if err := os.WriteFile(path, []byte(expanded), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", path)
	cmd.Env = append(e.environ(),
		"EVAL_HOME="+p.Home,
		"EVAL_WORKDIR="+p.Workdir,
		"EVAL_API_URL="+p.APIURL,
		"EVAL_WORKSPACE="+p.Workspace,
		"EVAL_SEEDS="+filepath.Dir(script),
		"TREBI_BIN="+e.bin,
	)
	cmd.Dir = e.workdir
	var errBuf bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("seed %s: %w\n%s", script, err, errBuf.Bytes())
	}
	return nil
}

func (e *trialEnv) writeBootConfig(harness, wsRoot string) error {
	if harness == "" {
		harness = defaultHarness
	}
	body := fmt.Sprintf("defaults:\n  command: %s\nworkspaces:\n  root: %q\n", harness, wsRoot)
	return os.WriteFile(filepath.Join(e.home, "config.yaml"), []byte(body), 0o644)
}

func (e *trialEnv) configure(ctx context.Context, harness string) error {
	_, err := e.trebi(ctx, "harness", "ensure", harness)
	return err
}

func (e *trialEnv) preflight(ctx context.Context, harness string) error {
	out, err := e.trebi(ctx, "doctor", "--json")
	if err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		return fmt.Errorf("doctor json: %w", err)
	}
	if doc["api_json_reachable"] != true || doc["daemon_lock_held"] != true {
		return fmt.Errorf("doctor not healthy: %s", out)
	}
	out, errb, err := e.runOut(ctx, e.bin, "harness", "auth", "status", "--json", "--live", harness)
	if !authOK(out) {
		if err != nil {
			return fmt.Errorf("auth status: %w\n%s%s", err, out, errb)
		}
		return fmt.Errorf("harness %s auth not ok: %s", harness, out)
	}
	return nil
}

func (e *trialEnv) snapshotHome() error {
	h, err := hashTree(e.home)
	if err != nil {
		return err
	}
	e.hashes = h
	return nil
}

func (e *trialEnv) workspacePath(ctx context.Context) string {
	out, err := e.trebi(ctx, "workspace", "current", "--json")
	if err != nil {
		return e.workdir
	}
	var st struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(out, &st) == nil && st.Path != "" {
		return st.Path
	}
	out, err = e.trebi(ctx, "workspace", "list", "--json")
	if err != nil {
		return e.workdir
	}
	var list struct {
		Workspaces []struct {
			Path string `json:"path"`
		} `json:"workspaces"`
	}
	if json.Unmarshal(out, &list) == nil && len(list.Workspaces) > 0 && list.Workspaces[0].Path != "" {
		return list.Workspaces[0].Path
	}
	return e.workdir
}

func authOK(raw []byte) bool {
	var rows []map[string]any
	if json.Unmarshal(raw, &rows) == nil {
		for _, r := range rows {
			st, _ := r["auth_status"].(string)
			if st == "" {
				st, _ = r["status"].(string)
			}
			if st == "ok" {
				return true
			}
		}
		return false
	}
	var one map[string]any
	if json.Unmarshal(raw, &one) != nil {
		return false
	}
	st, _ := one["auth_status"].(string)
	if st == "" {
		st, _ = one["status"].(string)
	}
	return st == "ok"
}

func filterEnv(env []string, drop ...string) []string {
	skip := map[string]bool{}
	for _, k := range drop {
		skip[k] = true
	}
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if skip[k] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func hashTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum, err := fileSHA(path)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = sum
		return nil
	})
	return out, err
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func daemonOwned(rel string) bool {
	rel = filepath.ToSlash(rel)
	owned := []string{
		"trebi.db", "logs/", "api.json", "daemon.lock", "trebid.pid",
		"state/", "harness/", "config.yaml", "context", "linked-workspaces.yaml",
		"skills/", "secrets/", "envsets.yaml", "integrations.yaml",
	}
	for _, p := range owned {
		trim := strings.TrimSuffix(p, "/")
		if rel == trim || strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}
