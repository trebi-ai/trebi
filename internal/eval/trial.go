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
	"sort"
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
	Infra    bool    `json:"infra,omitempty"`
	// Telemetry marks a dimension that reports but never fails a trial.
	Telemetry bool `json:"telemetry,omitempty"`
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
	Surface   string        `json:"surface,omitempty"`
	// ClientSkills fingerprints skills the client bootstrapped into the scratch
	// home. Uncontrolled input: it moves when the client ships a new skill.
	ClientSkills string `json:"client_skills,omitempty"`
}

// Passed reports deterministic checks and judge minima all green.
func (t *TrialResult) Passed() bool {
	return t != nil && t.Outcome == OutcomePass
}

func (t *TrialResult) writeFile() error {
	if t == nil || t.Dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(t.Dir, "trial.json"), data, 0o644)
}

// deadClientGate stops a trial whose client never produced an answer. A client
// that made tool calls but ran out of time is graded normally: its work is on
// the daemon and a retry would burn the same wall. A client that reported a
// fault instead is infra, and retries: it was cut off, not slow.
func (t *TrialResult) deadClientGate(launch *LaunchResult) bool {
	if launch != nil && launch.Steps.hasAssistant() {
		return false
	}
	if launch != nil && launch.Steps.toolCount() > 0 && len(launch.Errors) == 0 {
		t.Error = launch.errorSummary()
		return false
	}
	t.Outcome = OutcomeInfraFailure
	t.Error = launch.errorSummary()
	return true
}

func (t *TrialResult) failSummary() string {
	var parts []string
	for _, c := range t.Checks {
		if !c.Pass {
			parts = append(parts, "check "+c.Name+": "+c.Detail)
		}
	}
	for _, j := range t.Judge {
		if j.Telemetry {
			continue
		}
		if j.Infra {
			parts = append(parts, "judge "+j.Name+" infra")
			continue
		}
		if !j.Pass && !j.Unknown {
			parts = append(parts, "judge "+j.Name)
		}
	}
	return strings.Join(parts, "; ")
}

// trialEnv is one isolated TREBI_HOME + workdir + daemon child.
type trialEnv struct {
	home    string
	workdir string
	artDir  string
	apiURL  string
	token   string
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

// environ is the trial environment for seed scripts and check commands. They
// call trebi directly, so the trebi bin dir leads PATH.
func (e *trialEnv) environ() []string {
	env := os.Environ()
	env = filterEnv(env, "TREBI_HOME", "TREBI_URL", "TREBI_TOKEN", "TREBI_WORKSPACE")
	return append(env,
		"TREBI_HOME="+e.home,
		"PATH="+filepath.Dir(e.bin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
}

// cliEnviron is the environment of a cli trial. The client home is scratch, so
// the operator's own skills, AGENTS.md, and MCP servers stay out of the trial.
// The trebi bin dir leads PATH, because a cli trial must call trebi.
func (e *trialEnv) cliEnviron(cfgHome string) []string {
	env := e.clientHomeEnviron(cfgHome)
	return append(env,
		"TREBI_HOME="+e.home,
		"PATH="+filepath.Dir(e.bin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
}

// mcpEnviron is the environment of an mcp trial. It drops the trebi bin dir
// from PATH, so the client can only reach the daemon through /mcp.
func (e *trialEnv) mcpEnviron(cfgHome string) []string {
	env := e.clientHomeEnviron(cfgHome)
	return append(env, "PATH="+stripPathDir(os.Getenv("PATH"), filepath.Dir(e.bin)))
}

// clientHomeEnviron points the client at a scratch home, with no PATH set.
func (e *trialEnv) clientHomeEnviron(cfgHome string) []string {
	env := os.Environ()
	env = filterEnv(env, "TREBI_HOME", "TREBI_URL", "TREBI_TOKEN", "TREBI_WORKSPACE", "PATH", "HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME")
	return append(env,
		"HOME="+cfgHome,
		"CLAUDE_CONFIG_DIR="+cfgHome,
		"CODEX_HOME="+cfgHome,
	)
}

func stripPathDir(path, dir string) string {
	dir, _ = filepath.Abs(dir)
	var parts []string
	for _, p := range strings.Split(path, string(os.PathListSeparator)) {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err == nil && abs == dir {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, string(os.PathListSeparator))
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
				URL   string `json:"url"`
				Token string `json:"token"`
			}
			if json.Unmarshal(data, &doc) == nil && doc.URL != "" {
				e.apiURL = doc.URL
				e.token = doc.Token
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

func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// runWithGroup starts cmd in its own process group and SIGKILLs the group
// when ctx is done, so nested children (codex/claude workers) do not leak.
func runWithGroup(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		killProcGroup(cmd)
		<-done
		return ctx.Err()
	}
}

func seedClientHome(cfgHome, clientName string) error {
	name, _, _ := parseClientSpec(clientName)
	switch name {
	case clientCodex:
		return copyCodexAuth(cfgHome)
	case clientClaude:
		return seedIsolatedClaudeHome(cfgHome)
	default:
		return nil
	}
}

func copyCodexAuth(dst string) error {
	src := filepath.Join(realCodexHome(), "auth.json")
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, "auth.json"), data, 0o600)
}

// writeBackCodexAuth copies scratch auth.json over the real one when the
// scratch last_refresh is newer. Overlapping eval suites can still race a
// token rotation; trials in one run are sequential.
func writeBackCodexAuth(scratchHome string) error {
	if scratchHome == "" {
		return nil
	}
	realHome := realCodexHome()
	scratchAbs, _ := filepath.Abs(scratchHome)
	realAbs, _ := filepath.Abs(realHome)
	if realHome == "" || scratchAbs == realAbs {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(scratchHome, "auth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	scratchAt, ok := authLastRefresh(data)
	if !ok {
		return fmt.Errorf("scratch auth.json missing last_refresh")
	}
	realPath := filepath.Join(realHome, "auth.json")
	if !codexAuthNewer(scratchAt, realPath) {
		return nil
	}
	if err := atomicWriteFile(realPath, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "eval: wrote back codex auth.json\n")
	return nil
}

func codexAuthNewer(scratchAt time.Time, realPath string) bool {
	realData, err := os.ReadFile(realPath)
	if err != nil {
		return true
	}
	realAt, ok := authLastRefresh(realData)
	return !ok || scratchAt.After(realAt)
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func authLastRefresh(data []byte) (time.Time, bool) {
	var doc struct {
		LastRefresh string `json:"last_refresh"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.LastRefresh == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, doc.LastRefresh)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// seedIsolatedClaudeHome writes a minimal onboarding flag into an isolated
// scratch home so eval trials skip the first-run wizard without inheriting
// operator config. Differs from tmux.seedClaudeOnboarding, which merges
// keys from the real ~/.claude.json — that would leak personal settings
// into trial state and break reproducibility.
func seedIsolatedClaudeHome(dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, ".claude.json"), []byte(`{"hasCompletedOnboarding": true}`+"\n"), 0o644)
}

func realCodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
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

// clientSkillFingerprint hashes the skills a client bootstrapped into the
// scratch MCP home. Empty when it installed none.
func clientSkillFingerprint(cfgHome string) string {
	root := filepath.Join(cfgHome, "skills")
	tree, err := hashTree(root)
	if err != nil || len(tree) == 0 {
		return ""
	}
	rels := make([]string, 0, len(tree))
	for rel := range tree {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	names := map[string]bool{}
	h := sha256.New()
	for _, rel := range rels {
		fmt.Fprintf(h, "%s %s\n", rel, tree[rel])
		if parts := strings.Split(rel, "/"); len(parts) > 1 {
			names[parts[len(parts)-2]] = true
		}
	}
	return fmt.Sprintf("%d files, %d skills, sha256:%s",
		len(rels), len(names), hex.EncodeToString(h.Sum(nil))[:12])
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

// daemonOwned reports paths the daemon itself rewrites while a trial runs.
// Only those are exempt from "home untouched". A file the agent can edit by
// hand — config.yaml, skills, secrets, integrations — is not on this list,
// so a hand edit is a violation even when the daemon also writes the file.
func daemonOwned(rel string) bool {
	rel = filepath.ToSlash(rel)
	owned := []string{
		"trebi.db", "logs/", "api.json", "daemon.lock", "trebid.pid",
		"state/", "context", "harness/", "linked-workspaces.yaml",
	}
	for _, p := range owned {
		trim := strings.TrimSuffix(p, "/")
		if rel == trim || strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}
