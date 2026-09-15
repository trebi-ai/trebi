package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDialogKeyIgnoresCursor(t *testing.T) {
	t.Parallel()
	yes := "Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit\nEnter to confirm"
	no := "Is this a project you created or one you trust?\n  1. Yes, I trust this folder\n❯ 2. No, exit\nEnter to confirm"
	act := Action{Text: "1"}
	if DialogKey(yes, act) != DialogKey(no, act) {
		t.Fatal("cursor move must not change dialog identity")
	}
}

func TestTrustActionNumberedVsArrow(t *testing.T) {
	t.Parallel()
	if got := TrustAction("❯ 1. Yes, I trust this folder\n  2. No, exit"); got != (Action{Text: "1"}) {
		t.Fatalf("numbered: %+v", got)
	}
	if got := TrustAction("Trust project folder?\n→ Trust\n   Do not trust"); got != (Action{Key: "enter"}) {
		t.Fatalf("arrow: %+v", got)
	}
}

func TestChoiceDialog(t *testing.T) {
	t.Parallel()
	q, ok := ChoiceDialog("Allow network?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel")
	if !ok || !strings.Contains(q, "Allow network?") {
		t.Fatalf("got ok=%v q=%q", ok, q)
	}
	if _, ok := ChoiceDialog("❯ Try \"fix typecheck\"\n  Fable 5 | 0.5 min | 0K (0%)"); ok {
		t.Fatal("idle composer is not a choice dialog")
	}
	if _, ok := ChoiceDialog("Should I delete prod?"); ok {
		t.Fatal("bare question is not a choice dialog")
	}
}

func TestHandleStartup_ClaudeTrustCursorOnNo(t *testing.T) {
	t.Parallel()
	screen := "Is this a project you created or one you trust?\n  1. Yes, I trust this folder\n❯ 2. No, exit\nEnter to confirm"
	acts := (claudeTool{}).HandleStartup(screen)
	if len(acts) != 1 || acts[0].Text != "1" {
		t.Fatalf("want Text=1, got %+v", acts)
	}
	if (claudeTool{}).IsReady(screen) {
		t.Fatal("trust dialog must not be ready")
	}
}

func TestWaitReadyLoop_StuckTrustIsWaiting(t *testing.T) {
	t.Parallel()
	screen := "Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit\nEnter to confirm"
	clock := time.Unix(0, 0)
	var sent []Action
	io := readyIO{
		read:    func() (string, error) { return screen, nil },
		sendKey: func(key string) error { sent = append(sent, Action{Key: key}); return nil },
		send:    func(text string) error { sent = append(sent, Action{Text: text}); return nil },
		sleep:   func(d time.Duration) { clock = clock.Add(d) },
		now:     func() time.Time { return clock },
	}
	err := waitReadyLoop(claudeTool{}, io, 30*time.Second)
	if !errors.Is(err, ErrDialogWaiting) {
		t.Fatalf("want ErrDialogWaiting, got %v", err)
	}
	var dw *DialogWaitingError
	if !errors.As(err, &dw) || dw.Question == "" {
		t.Fatalf("want DialogWaitingError with question, got %#v", err)
	}
	if len(sent) != MaxDialogTries {
		t.Fatalf("sent %d times, want %d: %+v", len(sent), MaxDialogTries, sent)
	}
	for _, a := range sent {
		if a.Text != "1" {
			t.Fatalf("want Text=1, got %+v", a)
		}
	}
}

func TestWaitReadyLoop_UnknownChoiceIsWaiting(t *testing.T) {
	t.Parallel()
	screen := "Allow outbound network?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel"
	if len((claudeTool{}).HandleStartup(screen)) != 0 {
		t.Fatal("fixture must not be in the auto-answer catalog")
	}
	clock := time.Unix(0, 0)
	io := readyIO{
		read:  func() (string, error) { return screen, nil },
		sleep: func(d time.Duration) { clock = clock.Add(d) },
		now:   func() time.Time { return clock },
	}
	err := waitReadyLoop(claudeTool{}, io, 30*time.Second)
	if !errors.Is(err, ErrDialogWaiting) {
		t.Fatalf("want ErrDialogWaiting, got %v", err)
	}
}

func TestClaudeTrustGoldenCursorOnNo(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "screens", "claude", "startup-trust-no-default", "01-trust-folder.txt"))
	if err != nil {
		t.Fatal(err)
	}
	acts := (claudeTool{}).HandleStartup(string(data))
	if len(acts) != 2 || acts[0].Key != "down" || acts[1].Key != "enter" {
		t.Fatalf("golden actions %+v, want down then enter", acts)
	}
}

func TestTrustYesActionsUnnumberedNoFirst(t *testing.T) {
	t.Parallel()
	noFirst := "Is this a project you created or one you trust?\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm"
	acts := TrustYesActions(noFirst)
	if len(acts) != 2 || acts[0].Key != "down" || acts[1].Key != "enter" {
		t.Fatalf("no first: %+v", acts)
	}
	noBelow := "Is this a project you created or one you trust?\n  Yes, I trust this folder\n❯ No, exit\nEnter to confirm"
	acts = TrustYesActions(noBelow)
	if len(acts) != 2 || acts[0].Key != "up" || acts[1].Key != "enter" {
		t.Fatalf("no below: %+v", acts)
	}
	yes := "Is this a project you created or one you trust?\n❯ Yes, I trust this folder\n  No, exit\nEnter to confirm"
	acts = TrustYesActions(yes)
	if len(acts) != 1 || acts[0].Key != "enter" {
		t.Fatalf("yes highlighted: %+v", acts)
	}
}
