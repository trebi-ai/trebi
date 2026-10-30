package visor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// LabelPane holds the name of a pane session.
const LabelPane = "trebi.pane"

// PaneSpec opens one pane: a login shell in a PTY session.
type PaneSpec struct {
	// Name finds the pane again (Find). It is optional.
	Name   string
	Title  string
	Dir    string
	Env    map[string]string
	Labels map[string]string
}

// Panes are the terminal sessions of the tui runtime and the harness login.
// A pane runs a login shell, so it stays when the program in it ends.
type Panes struct {
	host func(context.Context) (Host, error)
}

// NewPanes gives the panes of the visor that host returns.
func NewPanes(host func(context.Context) (Host, error)) *Panes {
	return &Panes{host: host}
}

// Open starts a pane and gives its session id.
func (p *Panes) Open(ctx context.Context, spec PaneSpec) (string, error) {
	h, err := p.host(ctx)
	if err != nil {
		return "", err
	}
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	if spec.Name != "" {
		labels[LabelPane] = spec.Name
	}
	s, err := h.Start(ctx, agentvisor.StartSpec{
		Kind: agentvisor.KindPTY, Role: agentvisor.RoleShell, Title: spec.Title,
		WorkingDir: spec.Dir, Env: spec.Env, Labels: labels,
	})
	if err != nil {
		return "", fmt.Errorf("open a pane: %w", err)
	}
	return s.ID, nil
}

// Find gives the live pane with name.
func (p *Panes) Find(ctx context.Context, name string) (string, bool) {
	ss, err := p.List(ctx, map[string]string{LabelPane: name})
	if err != nil || len(ss) == 0 {
		return "", false
	}
	return ss[0].ID, true
}

// List gives the live panes that have all labels.
func (p *Panes) List(ctx context.Context, labels map[string]string) ([]agentvisor.Session, error) {
	h, err := p.host(ctx)
	if err != nil {
		return nil, err
	}
	ss, err := h.Sessions(ctx, agentvisor.Filter{Kind: agentvisor.KindPTY, Labels: labels})
	if err != nil {
		return nil, err
	}
	live := ss[:0]
	for _, s := range ss {
		if s.State.Live() {
			live = append(live, s)
		}
	}
	return live, nil
}

// pasteAbove is the length from which Send pastes a line, not types it.
const pasteAbove = 4096

// Send types text. Text with a newline, or a long line, goes as a paste. A
// trailing newline presses Enter after the text, because a newline in a
// bracketed paste does not submit.
func (p *Panes) Send(ctx context.Context, id, text string) error {
	h, err := p.host(ctx)
	if err != nil {
		return err
	}
	body, submit := strings.CutSuffix(text, "\n")
	body = strings.TrimSuffix(body, "\r")
	switch {
	case body == "":
	case len(body) > pasteAbove || strings.ContainsAny(body, "\n\r"):
		err = h.Paste(ctx, id, body)
	default:
		err = h.Keys(ctx, id, []byte(body))
	}
	if err != nil {
		return fmt.Errorf("type into pane %s: %w", id, err)
	}
	if submit {
		return h.Keys(ctx, id, []byte("\r"))
	}
	return nil
}

// Key presses one named key, for example "enter" or "c-c". Other text goes
// as typed.
func (p *Panes) Key(ctx context.Context, id, key string) error {
	h, err := p.host(ctx)
	if err != nil {
		return err
	}
	if err := h.Keys(ctx, id, []byte(keyBytes(key))); err != nil {
		return fmt.Errorf("press %s in pane %s: %w", key, id, err)
	}
	return nil
}

// Screen gives the visible rows of the pane.
func (p *Panes) Screen(ctx context.Context, id string) (string, error) {
	h, err := p.host(ctx)
	if err != nil {
		return "", err
	}
	sc, err := h.Screen(ctx, id)
	if err != nil {
		return "", fmt.Errorf("read pane %s: %w", id, err)
	}
	return strings.Join(sc.Lines, "\n") + "\n", nil
}

// History gives the recent output of the pane as text, also after it ends.
func (p *Panes) History(ctx context.Context, id string) (string, error) {
	h, err := p.host(ctx)
	if err != nil {
		return "", err
	}
	out, err := h.Output(ctx, id)
	if err != nil {
		return "", fmt.Errorf("read the output of pane %s: %w", id, err)
	}
	return out, nil
}

// Alive reports if the pane runs.
func (p *Panes) Alive(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	h, err := p.host(ctx)
	if err != nil {
		return false
	}
	s, err := h.Session(ctx, id)
	return err == nil && s.State.Live()
}

// Kill stops the pane. A pane that ended already is not an error.
func (p *Panes) Kill(ctx context.Context, id string) error {
	h, err := p.host(ctx)
	if err != nil {
		return err
	}
	if err := h.Stop(ctx, id); err != nil && !errors.Is(err, agentvisor.ErrNotFound) && !errors.Is(err, agentvisor.ErrState) {
		return fmt.Errorf("stop pane %s: %w", id, err)
	}
	return nil
}

// Pane binds one pane, for code that reads and types into one terminal.
func (p *Panes) Pane(id string) Pane { return Pane{panes: p, ID: id} }

// Pane is one pane of Panes.
type Pane struct {
	panes *Panes
	ID    string
}

func (p Pane) Screen(ctx context.Context) (string, error)  { return p.panes.Screen(ctx, p.ID) }
func (p Pane) Send(ctx context.Context, text string) error { return p.panes.Send(ctx, p.ID, text) }
func (p Pane) Key(ctx context.Context, key string) error   { return p.panes.Key(ctx, p.ID, key) }

// keyBytes maps a key name to the bytes that a terminal sends for it. The
// names are the ones of the attach clients, for example "Up", "BSpace", and
// "C-c".
func keyBytes(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if len(k) == 3 && k[:2] == "c-" && k[2] >= 'a' && k[2] <= 'z' {
		return string(rune(k[2] - 'a' + 1))
	}
	if b, ok := map[string]string{
		"enter": "\r", "return": "\r", "tab": "\t", "escape": "\x1b", "esc": "\x1b", "space": " ",
		"ctrl-c": "\x03", "ctrl-d": "\x04", "bspace": "\x7f", "backspace": "\x7f",
		"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
		"dc": "\x1b[3~", "delete": "\x1b[3~", "home": "\x1b[H", "end": "\x1b[F",
		"pageup": "\x1b[5~", "pagedown": "\x1b[6~",
	}[k]; ok {
		return b
	}
	return key
}
