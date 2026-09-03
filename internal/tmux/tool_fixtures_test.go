package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"
)

// requireAllTools: every tool must have at least one screen scenario —
// missing fixtures fail instead of skipping.
const requireAllTools = true

// frameRe matches fixture frame filenames: "<NN>-<state>.txt".
var frameRe = regexp.MustCompile(`^(\d+)-([a-z-]+)\.txt$`)

// screenFrame is one captured pane snapshot within a scenario.
type screenFrame struct {
	path    string
	index   int
	state   string
	content string
}

// frameStates is the fixed state vocabulary; the filename is the expectation.
// An unrecognized state fails the walk (typo guard). midSession frames assert
// the coarse screen classifier instead of readiness.
var frameStates = map[string]struct{ midSession bool }{
	"boot":               {},
	"trust-folder":       {},
	"bypass-permissions": {},
	"hooks-trust":        {},
	"setup":              {},
	"picker":             {},
	"ready":              {},
	"busy":               {midSession: true},
	"idle":               {midSession: true},
}

// hooksTrustActions holds the per-tool expected actions on a hooks-trust
// dialog, filled in once a real capture shows one.
var hooksTrustActions = map[string][]Action{}

// setupActions holds the per-tool expected actions on a "setup" dialog frame
// (first-run offers that are neither trust nor bypass).
var setupActions = map[string][]Action{
	Copilot: {{Key: "escape"}}, // decline the VS Code keybinding offer
}

func TestScreenFixtures(t *testing.T) {
	t.Parallel()
	walkScreenFixtures(t, filepath.Join("testdata", "screens"), requireAllTools)
}

// --- walker self-tests: prove the assertions execute -----------------------

const (
	selfTestTrustScreen  = "Is this a project you created or one you trust?\n  1. Yes, I trust this folder\n  2. No"
	selfTestBypassScreen = "Bypass Permissions mode\n  1. No\n  2. Yes, I accept"
	selfTestReadyScreen  = "❯ \n  Fable 5 | 0.0 min | 0K (0%) | /tmp/x"
)

func TestScreenFixtureWalker_SelfTest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSelfTestScenario(t, filepath.Join(root, "claude", "startup"), map[string]string{
		"01-boot.txt":               "Welcome to Claude Code!\nLoading...",
		"02-trust-folder.txt":       selfTestTrustScreen,
		"03-trust-folder.txt":       selfTestTrustScreen, // re-rendered identical dialog: must not re-fire
		"04-bypass-permissions.txt": selfTestBypassScreen,
		"05-ready.txt":              selfTestReadyScreen,
		"info.txt":                  "synthetic",
	})

	checked, replayed := walkScreenFixtures(t, root, false)
	if checked != 5 {
		t.Errorf("framesChecked = %d, want 5 — walker skipped frames silently", checked)
	}
	if replayed != 1 {
		t.Errorf("scenariosReplayed = %d, want 1", replayed)
	}
}

func TestScreenFixtureChecks_RejectBadFrames(t *testing.T) {
	t.Parallel()
	tool := claudeTool{}
	if err := checkFrame(tool, screenFrame{path: "x/05-ready.txt", state: "ready", content: "splash art"}); err == nil {
		t.Error("ready frame that is not IsReady must fail")
	}
	if err := checkFrame(tool, screenFrame{path: "x/01-boot.txt", state: "boot", content: selfTestReadyScreen}); err == nil {
		t.Error("boot frame reported ready must fail (splash-as-ready)")
	}
	if err := checkFrame(tool, screenFrame{path: "x/02-trust-folder.txt", state: "trust-folder", content: "no dialog here"}); err == nil {
		t.Error("trust-folder frame with no action must fail")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "01-reddy.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadScenario(dir); err == nil {
		t.Error("unrecognized state name must fail loadScenario")
	}
}

func TestReplayScenario_NoReadyFrame(t *testing.T) {
	t.Parallel()
	// A scenario with no ready frame (e.g. a resume picker) must time out —
	// never paste.
	frames := []screenFrame{{path: "01-boot.txt", state: "boot", content: "Loading..."}}
	if err := replayScenario(claudeTool{}, frames); err != nil {
		t.Errorf("no-ready scenario should validate as timeout, got: %v", err)
	}

	// A non-ready frame the tool treats as ready must be caught.
	bad := []screenFrame{{path: "01-boot.txt", state: "boot", content: selfTestReadyScreen}}
	if err := replayScenario(claudeTool{}, bad); err == nil {
		t.Error("expected replay error when the loop exits ready with no ready frame")
	}
}

func TestWaitReadyLoop_ReadErrorsThenTimeout(t *testing.T) {
	t.Parallel()
	clock := time.Unix(0, 0)
	reads := 0
	io := readyIO{
		read:  func() (string, error) { reads++; return "", fmt.Errorf("no pane") },
		sleep: func(d time.Duration) { clock = clock.Add(d) },
		now:   func() time.Time { return clock },
	}
	err := waitReadyLoop(claudeTool{}, io, 5*time.Second)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if reads == 0 {
		t.Error("loop should keep polling through read errors")
	}
}

func TestWaitReadyLoop_AuthBlockerFailsFast(t *testing.T) {
	t.Parallel()
	// Login chrome still has ❯ + " min | " (would look ready without auth check).
	login := "Not logged in · Please run /login\n❯ \n  Opus 5 | 0.3 min | 0K (0%) | ~/proj\n"
	if claudeReady(login) {
		t.Fatal("claudeReady must reject login screen")
	}
	if !IsAuthBlocker(login) {
		t.Fatal("IsAuthBlocker should match login screen")
	}

	clock := time.Unix(0, 0)
	reads := 0
	io := readyIO{
		read: func() (string, error) {
			reads++
			return login, nil
		},
		sleep: func(d time.Duration) { clock = clock.Add(d) },
		now:   func() time.Time { return clock },
	}
	err := waitReadyLoop(claudeTool{}, io, 30*time.Second)
	if err == nil {
		t.Fatal("expected auth missing error")
	}
	if !errors.Is(err, ErrAuthMissing) {
		t.Fatalf("want ErrAuthMissing, got %v", err)
	}
	if reads > 3 {
		t.Fatalf("should fail fast, reads=%d", reads)
	}
}

func TestIsAuthBlocker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		screen string
		want   bool
	}{
		{"", false},
		{"Hello working on task", false},
		{"Not logged in · Please run /login", true},
		{"  Not logged in · Run /login  ", true},
		{"checking login status in docs", false},
		// Agent / site-login prose must not match (utilities false positive).
		{"Login required with prompt=login (MFA flow)", false},
		{"Authentication required to view bill", false},
		{"please log in to the portal", false},
	}
	for _, tc := range cases {
		if got := IsAuthBlocker(tc.screen); got != tc.want {
			t.Errorf("IsAuthBlocker(%q)=%v want %v", tc.screen, got, tc.want)
		}
	}
	if ok, reason := AuthBlockerMatch("Not logged in · Please run /login"); !ok || reason == "" {
		t.Fatalf("AuthBlockerMatch: ok=%v reason=%q", ok, reason)
	}
}

func TestWaitReadyLoop_LimitBlockerFailsFast(t *testing.T) {
	t.Parallel()
	// Session-limit chrome still has ❯ + " min | " (would look ready without check).
	limit := "You've hit your session limit · resets 12:20pm (America/Sao_Paulo)\n" +
		"/upgrade to increase your usage limit.\n❯ \n  Opus 5 | 0.3 min | 0K (0%) | ~/proj\n"
	if claudeReady(limit) {
		t.Fatal("claudeReady must reject limit screen")
	}
	if !IsLimitBlocker(limit) {
		t.Fatal("IsLimitBlocker should match session limit screen")
	}

	clock := time.Unix(0, 0)
	reads := 0
	io := readyIO{
		read: func() (string, error) {
			reads++
			return limit, nil
		},
		sleep: func(d time.Duration) { clock = clock.Add(d) },
		now:   func() time.Time { return clock },
	}
	err := waitReadyLoop(claudeTool{}, io, 30*time.Second)
	if err == nil {
		t.Fatal("expected limit exceeded error")
	}
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("want ErrLimitExceeded, got %v", err)
	}
	if reads > 3 {
		t.Fatalf("should fail fast, reads=%d", reads)
	}
}

func TestIsLimitBlocker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		screen string
		want   bool
	}{
		{"", false},
		{"Hello working on task", false},
		{"You've hit your session limit · resets 12:20pm", true},
		{"/upgrade to increase your usage limit.", true},
		{"respect the API rate limit when calling", false},
		{"usage limit on their plan docs", false},
	}
	for _, tc := range cases {
		if got := IsLimitBlocker(tc.screen); got != tc.want {
			t.Errorf("IsLimitBlocker(%q)=%v want %v", tc.screen, got, tc.want)
		}
	}
	if ok, reason := LimitBlockerMatch("You've hit your session limit"); !ok || reason == "" {
		t.Fatalf("LimitBlockerMatch: ok=%v reason=%q", ok, reason)
	}
}

// checkFrame asserts the IsReady/HandleStartup contract for one pre-paste frame.
func checkFrame(tool Tool, f screenFrame) error {
	ready := tool.IsReady(f.content)
	acts := tool.HandleStartup(f.content)

	if f.state != "ready" && ready {
		return fmt.Errorf("%s: %s frame reported IsReady — paste would fire early", f.path, f.state)
	}

	switch f.state {
	case "ready":
		if !ready {
			return fmt.Errorf("%s: ready frame not IsReady", f.path)
		}
		if len(acts) != 0 {
			return fmt.Errorf("%s: ready frame produced startup actions %+v", f.path, acts)
		}
	case "boot", "picker":
		if len(acts) != 0 {
			return fmt.Errorf("%s: %s frame produced startup actions %+v", f.path, f.state, acts)
		}
	case "trust-folder":
		if len(acts) != 1 || acts[0].Key != "enter" || acts[0].Text != "" {
			return fmt.Errorf("%s: trust-folder actions = %+v, want [{Key:enter}]", f.path, acts)
		}
	case "bypass-permissions":
		if len(acts) != 1 || acts[0].Text != "2" || acts[0].Key != "" {
			return fmt.Errorf("%s: bypass-permissions actions = %+v, want [{Text:2}]", f.path, acts)
		}
	case "hooks-trust":
		want, known := hooksTrustActions[tool.Name()]
		if known && !actionsEqual(acts, want) {
			return fmt.Errorf("%s: hooks-trust actions = %+v, want %+v", f.path, acts, want)
		}
	case "setup":
		want, known := setupActions[tool.Name()]
		if !known {
			return fmt.Errorf("%s: setup frame but no setupActions entry for %s", f.path, tool.Name())
		}
		if !actionsEqual(acts, want) {
			return fmt.Errorf("%s: setup actions = %+v, want %+v", f.path, acts, want)
		}
	}
	return nil
}

// checkClassifierFrame asserts busy/idle frames against the coarse screen
// classifier, and that mid-session frames never re-enter the readiness path
// via HandleStartup.
func checkClassifierFrame(tool Tool, f screenFrame) error {
	if acts := tool.HandleStartup(f.content); len(acts) != 0 {
		return fmt.Errorf("%s: mid-session %s frame produced startup actions %+v", f.path, f.state, acts)
	}
	c, ok := tool.(ScreenClassifier)
	if !ok {
		return fmt.Errorf("%s: mid-session frame but %s implements no ScreenClassifier", f.path, tool.Name())
	}
	want := ScreenClass(f.state)
	if got := c.ClassifyScreen(f.content); got != want {
		return fmt.Errorf("%s: ClassifyScreen = %s, want %s", f.path, got, want)
	}
	return nil
}

func actionsEqual(a, b []Action) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// replayScenario feeds pre-paste frames through waitReadyLoop: each read pops
// the next frame, the last frame repeats. Asserts the loop exits ready exactly
// on the first ready-labeled frame (or times out when the scenario has none),
// and that each distinct dialog fires its action exactly once.
func replayScenario(tool Tool, frames []screenFrame) error {
	readyIdx := -1
	for i, f := range frames {
		if f.state == "ready" {
			readyIdx = i
			break
		}
	}

	reads := 0
	var sent []Action
	clock := time.Unix(0, 0)
	io := readyIO{
		read: func() (string, error) {
			i := reads
			if i >= len(frames) {
				i = len(frames) - 1
			}
			reads++
			return frames[i].content, nil
		},
		sendKey: func(key string) error { sent = append(sent, Action{Key: key}); return nil },
		send:    func(text string) error { sent = append(sent, Action{Text: text}); return nil },
		sleep:   func(d time.Duration) { clock = clock.Add(d) },
		now:     func() time.Time { return clock },
	}

	// Fake-clock budget: every iteration advances >= 1s, so this terminates.
	timeout := time.Duration(len(frames)+20) * 3 * time.Second
	err := waitReadyLoop(tool, io, timeout)

	if readyIdx == -1 {
		if err == nil {
			return fmt.Errorf("scenario has no ready frame but the loop reported ready — paste would fire")
		}
	} else {
		if err != nil {
			return fmt.Errorf("loop never reached ready: %v", err)
		}
		if reads != readyIdx+1 {
			return fmt.Errorf("loop exited after %d reads, want %d (first ready frame %s)",
				reads, readyIdx+1, frames[readyIdx].path)
		}
	}

	// Expected fires: one per distinct (action, dialog fingerprint) among the
	// frames the loop visited. Identical re-rendered dialogs collapse to one.
	visited := frames
	if readyIdx >= 0 {
		visited = frames[:readyIdx+1]
	}
	wantFires := map[string]bool{}
	for _, f := range visited {
		for _, act := range tool.HandleStartup(f.content) {
			wantFires[act.Key+"\x00"+act.Text+"\x00"+screenFingerprint(f.content, act)] = true
		}
	}
	if len(sent) != len(wantFires) {
		return fmt.Errorf("actions fired %d times, want %d distinct dialog fires (sent %+v)",
			len(sent), len(wantFires), sent)
	}
	return nil
}

// loadScenario reads a scenario directory into ordered frames.
func loadScenario(dir string) ([]screenFrame, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var frames []screenFrame
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name[0] == '.' || name == "info.txt" {
			continue
		}
		full := filepath.Join(dir, name)
		m := frameRe.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("%s: frame name must be <NN>-<state>.txt", full)
		}
		if _, ok := frameStates[m[2]]; !ok {
			return nil, fmt.Errorf("%s: unrecognized state %q (see frameStates)", full, m[2])
		}
		idx, _ := strconv.Atoi(m[1])
		content, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		frames = append(frames, screenFrame{path: full, index: idx, state: m[2], content: string(content)})
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("%s: no frames", dir)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].index < frames[j].index })
	return frames, nil
}

// listScenarioDirs returns sorted scenario dirs under a tool's fixture dir.
func listScenarioDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '_' && e.Name()[0] != '.' && e.Name() != "login" {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// walkScreenFixtures runs frame predicate checks and loop replay for every
// scenario under root. Returns counts so the self-test can prove assertions
// actually executed.
func walkScreenFixtures(t *testing.T, root string, requireAll bool) (framesChecked, scenariosReplayed int) {
	t.Helper()
	for _, tool := range AllTools() {
		tool := tool
		t.Run(tool.Name(), func(t *testing.T) {
			dir := filepath.Join(root, tool.Name())
			scenarios := listScenarioDirs(dir)
			if len(scenarios) == 0 {
				// The fake harness is a CLI stand-in with no TUI to capture.
				if requireAll && tool.Name() != Fake {
					t.Fatalf("no screen fixtures for %s under %s (requireAllTools is set)", tool.Name(), dir)
				}
				t.Skipf("no screen fixtures for %s under %s — capture with scripts/capture-tui", tool.Name(), dir)
			}
			for _, sc := range scenarios {
				frames, err := loadScenario(sc)
				if err != nil {
					t.Errorf("loading scenario: %v", err)
					continue
				}
				var pre []screenFrame
				for _, f := range frames {
					if frameStates[f.state].midSession {
						if err := checkClassifierFrame(tool, f); err != nil {
							t.Error(err)
						}
						framesChecked++
						continue
					}
					if err := checkFrame(tool, f); err != nil {
						t.Error(err)
					}
					framesChecked++
					pre = append(pre, f)
				}
				if len(pre) > 0 {
					if err := replayScenario(tool, pre); err != nil {
						t.Errorf("%s: replay: %v", sc, err)
					}
					scenariosReplayed++
				}
			}
		})
	}
	return
}

func writeSelfTestScenario(t *testing.T, dir string, frames map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range frames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
