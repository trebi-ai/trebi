package tmux

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

type codexTool struct{ harnessHome }

func (codexTool) Name() string { return Codex }

func (codexTool) Match(command string) bool {
	return binaryMatch(command, Codex)
}

func (t codexTool) PrepareLaunch(command string) LaunchPlan {
	// Soft-event only (agent-turn-complete → stop) so screen still drives busy.
	// Quote the TOML array so bash does not strip the quotes and Codex 0.149+
	// does not deserialize notify as a string.
	notifyBin := filepath.Join(config.LibexecDir(), "trebi-codex-notify")
	cmd := command
	if _, err := os.Stat(notifyBin); err == nil {
		cmd = AppendFlags(cmd, []string{
			"-c", fmt.Sprintf(`'notify=["%s"]'`, notifyBin),
		})
	}
	t.tool = Codex
	// Do not set CODEX_HOME: a fresh harness tree cold-starts Codex plugin
	// sync and misses WaitReady. Notify is injected via -c; auth stays in
	// the user ~/.codex. Bridges remain dormant until login isolation.
	_, _ = t.prepare()
	return LaunchPlan{Command: cmd, Env: nil}
}

func (codexTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID captured for codex resume")
	}
	return LaunchPlan{Command: AppendFlags(command, []string{"--resume", sessionID}), SessionID: sessionID}, nil
}

// codexTrustDialog marks the first-run folder-trust dialog; its text starts
// with "> You are in ..." so it must be checked before the composer glyph.
const codexTrustDialog = "Do you trust the contents of this directory"

func (codexTool) IsReady(screen string) bool {
	// Composer glyph "› " (goldens: testdata/screens/codex/startup). Renders
	// while "model: loading" — typing is buffered, so that still counts.
	if strings.Contains(screen, codexTrustDialog) {
		return false
	}
	return strings.Contains(screen, "› ")
}

// HandleStartup: trust dialog — pick "1. Yes, continue", not the highlight.
func (codexTool) HandleStartup(screen string) []Action {
	if strings.Contains(screen, codexTrustDialog) {
		return []Action{{Text: "1"}}
	}
	return nil
}
func (codexTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

// codexStartSkew allows for the gap between the run row and the rollout header.
// Codex writes session_meta after trebi records the start.
const codexStartSkew = 2 * time.Minute

// CodexWorkingDir encodes a working directory and a run start time for
// TranscriptPath. Codex mints its own session id, so its rollout is found by
// path; the start time separates two runs that share that path.
func CodexWorkingDir(dir string, startedAt time.Time) string {
	if dir == "" || startedAt.IsZero() {
		return dir
	}
	return dir + "\n" + startedAt.UTC().Format(time.RFC3339Nano)
}

// TranscriptPath finds the rollout file for one codex session. Codex mints its
// own session id and files rollouts under sessions/<yyyy>/<mm>/<dd>/, so the
// path cannot be computed: it is found by matching the id in the file name.
func (codexTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session id recorded")
	}
	workingDir, startedAt := splitCodexWorkingDir(workingDir)
	roots := codexSessionRoots(home)
	// A harness that files rollouts under its own id lets the id match directly.
	// Codex does not, so this loop usually finds nothing and the fallback runs.
	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "20*", "*", "*", "rollout-*"+sessionID+".jsonl"))
		if err != nil {
			return "", err
		}
		for _, m := range matches {
			if fileExistsNotDir(m) {
				return m, nil
			}
		}
	}
	// Codex mints its own rollout id, so a trebi session id never matches a
	// file name. Fall back to the newest rollout started in this working dir.
	if workingDir != "" {
		if p := newestCodexRolloutFor(roots, workingDir, startedAt); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("no rollout file for session %s", sessionID)
}

// codexRolloutMaxAge bounds the rollout search. A run being ingested is live or
// just ended, so an older file cannot be its session.
const codexRolloutMaxAge = 24 * time.Hour

func (codexTool) ParseTranscript(r io.Reader) ([]Turn, error) {
	return parseCodexTranscript(r)
}

func (codexTool) Lifecycle() LifecycleAdapter { return codexLifecycle{} }

//go:embed lifecycle/trebi-codex-notify.sh
var codexNotifyScript []byte

type codexLifecycle struct{}

func (codexLifecycle) Install(trebiBase, binDir string) error {
	// Helpers live under libexec (not bin/) so they never collide with
	// versioned stores at bin/<tool>/<ver>/.
	libexec := filepath.Join(trebiBase, "libexec")
	if err := os.MkdirAll(libexec, 0755); err != nil {
		return err
	}
	path := filepath.Join(libexec, "trebi-codex-notify")
	if err := os.WriteFile(path, codexNotifyScript, 0755); err != nil {
		return fmt.Errorf("writing codex notify helper: %w", err)
	}
	// Remove legacy flat install under bin/ if present.
	legacy := filepath.Join(binDir, "trebi-codex-notify")
	_ = os.Remove(legacy)
	// Optional isolated home for strategy B (auth bridge prepared on demand).
	dir := filepath.Join(trebiBase, "harness", Codex)
	if err := harness.EnsureDir(dir); err != nil {
		return err
	}
	return harness.WriteSchemaStamp(dir)
}

func (codexLifecycle) Uninstall(trebiBase, binDir string) error {
	for _, path := range []string{
		filepath.Join(trebiBase, "libexec", "trebi-codex-notify"),
		filepath.Join(binDir, "trebi-codex-notify"), // legacy
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// codexRollout is one line of a codex rollout file.
type codexRollout struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Name    string `json:"name"`
		CallID  string `json:"call_id"`
		Args    string `json:"arguments"`
		Output  string `json:"output"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"payload"`
}

// parseCodexTranscript reads a codex rollout file into turns. Only user and
// assistant messages become turns: the developer role carries the harness
// scaffolding, and reasoning is not part of the conversation.
func parseCodexTranscript(r io.Reader) ([]Turn, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	turns := []Turn{}
	outputs := map[string]string{}
	var pending []Turn
	// callIDs[i] is the call id of pending[i] when that turn is a tool call;
	// the output arrives on a later line and is matched back by id.
	var callIDs []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw codexRollout
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if raw.Type != "response_item" {
			continue
		}
		switch raw.Payload.Type {
		case "message":
			t, ok := raw.messageTurn()
			if ok {
				pending = append(pending, t)
				callIDs = append(callIDs, "")
			}
		case "function_call":
			pending = append(pending, Turn{
				Role: "tools", At: parseClaudeTime(raw.Timestamp),
				Tools: []ToolCall{{Name: raw.Payload.Name, Input: raw.Payload.Args}},
			})
			callIDs = append(callIDs, raw.Payload.CallID)
		case "function_call_output":
			outputs[raw.Payload.CallID] = raw.Payload.Output
		}
	}
	for i, t := range pending {
		if id := callIDs[i]; id != "" && len(t.Tools) == 1 {
			t.Tools[0].Output = outputs[id]
		}
		turns = append(turns, t)
	}
	return turns, sc.Err()
}

// messageTurn converts one message payload. Text parts are named by direction:
// input_text for a user turn, output_text for an assistant turn.
func (r codexRollout) messageTurn() (Turn, bool) {
	role := r.Payload.Role
	if role != "user" && role != "assistant" {
		return Turn{}, false
	}
	var b strings.Builder
	for _, c := range r.Payload.Content {
		if c.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(c.Text)
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return Turn{}, false
	}
	return Turn{Role: role, At: parseClaudeTime(r.Timestamp), Text: text}, true
}

// newestCodexRolloutFor returns the most recent rollout whose session_meta cwd
// is workingDir. Codex records the cwd on its first line, so one short read
// identifies a session. Candidates are ordered newest first and the scan stops
// at the first match, so the common case opens one file.
func newestCodexRolloutFor(roots []string, workingDir string, startedAt time.Time) string {
	type cand struct {
		path string
		mod  time.Time
	}
	cutoff := time.Now().Add(-codexRolloutMaxAge)
	var cands []cand
	for _, root := range roots {
		matches, err := codexRecentRollouts(root, cutoff)
		if err != nil {
			continue
		}
		for _, m := range matches {
			st, err := os.Stat(m)
			if err != nil || st.IsDir() || st.ModTime().Before(cutoff) {
				continue
			}
			cands = append(cands, cand{m, st.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	best, bestAt := "", time.Time{}
	for _, c := range cands {
		cwd, at := codexRolloutMeta(c.path)
		if cwd != workingDir {
			continue
		}
		// Several runs of one assistant share a working directory. Keep the
		// rollout that started first at or after the run, and fall back to the
		// newest when no start time is known.
		if !startedAt.IsZero() {
			if at.IsZero() || at.Before(startedAt.Add(-codexStartSkew)) {
				continue
			}
			if best == "" || at.Before(bestAt) {
				best, bestAt = c.path, at
			}
			continue
		}
		return c.path
	}
	return best
}

// codexRolloutMeta reads the cwd and start time from a rollout's session_meta
// first line. The time separates two rollouts that share a working directory.
func codexRolloutMeta(path string) (string, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		return "", time.Time{}
	}
	var head struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &head) != nil || head.Type != "session_meta" {
		return "", time.Time{}
	}
	at, _ := time.Parse(time.RFC3339Nano, head.Payload.Timestamp)
	return head.Payload.Cwd, at
}

// codexSessionRoots lists the session stores to search, most specific first.
// harnessDir is the trebi harness home; codex writes there only when it was
// launched with CODEX_HOME set. PrepareLaunch leaves CODEX_HOME alone, so the
// user store holds the rollouts of every run and must stay in the list.
func codexSessionRoots(harnessDir string) []string {
	var roots []string
	if h := os.Getenv("CODEX_HOME"); h != "" {
		roots = append(roots, filepath.Join(h, "sessions"))
	}
	if harnessDir != "" {
		roots = append(roots, filepath.Join(harnessDir, "sessions"))
	}
	roots = append(roots, filepath.Join(config.HarnessDir(Codex), "sessions"))
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		roots = append(roots, filepath.Join(h, ".codex", "sessions"))
	}
	return dedupeExisting(roots)
}

// dedupeExisting drops repeats and stores that are not present, so a scan does
// not open the same rollout twice under two names.
func dedupeExisting(roots []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			abs = r
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			out = append(out, abs)
		}
	}
	return out
}

// codexRecentRollouts globs only the day directories the cutoff can reach. A
// user store holds thousands of rollouts across years; walking all of them on
// every ingest costs more than the whole read.
func codexRecentRollouts(root string, cutoff time.Time) ([]string, error) {
	var out []string
	for d := time.Now().UTC(); !d.Before(cutoff.UTC().AddDate(0, 0, -1)); d = d.AddDate(0, 0, -1) {
		day := filepath.Join(root, d.Format("2006"), d.Format("01"), d.Format("02"))
		matches, err := filepath.Glob(filepath.Join(day, "rollout-*.jsonl"))
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
	}
	return out, nil
}

// splitCodexWorkingDir undoes CodexWorkingDir. A plain directory still works, so
// a caller that knows no start time keeps the newest-rollout behaviour.
func splitCodexWorkingDir(v string) (string, time.Time) {
	dir, ts, ok := strings.Cut(v, "\n")
	if !ok {
		return v, time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return dir, time.Time{}
	}
	return dir, at
}
