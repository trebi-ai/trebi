package visor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// ErrNoHooks is a harness that cannot report its sessions.
var ErrNoHooks = errors.New("this harness cannot show its sessions")

// ObservedSession is an agent session that the user started outside Trebi.
// agent-visor sees it through the vendor hooks.
type ObservedSession struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Folder  string `json:"folder"`
	// State is starting, running, waiting, idle, lost, or exited.
	State        string    `json:"state"`
	Turns        int       `json:"turns"`
	Waiting      int       `json:"waiting"`
	LastActivity time.Time `json:"last_activity"`
	StartedAt    time.Time `json:"started_at"`
}

// ObservedTurn is one entry of the transcript of an observed session.
type ObservedTurn struct {
	At time.Time `json:"at"`
	// Role is user, assistant, request, or error.
	Role string `json:"role"`
	Text string `json:"text"`
}

// Hook states of a harness.
const (
	HooksOn          = "on"
	HooksOff         = "off"
	HooksOutdated    = "outdated"
	HooksUntrusted   = "untrusted"
	HooksUnsupported = "unsupported"
)

// hookHost is the part of agent-visor that installs the vendor hooks.
type hookHost interface {
	Hooks(ctx context.Context, spec agentvisor.HookSpec) ([]agentvisor.HookStatus, error)
	InstallHooks(ctx context.Context, spec agentvisor.HookSpec) ([]agentvisor.HookStatus, error)
	RemoveHooks(ctx context.Context, spec agentvisor.HookSpec) ([]agentvisor.HookStatus, error)
}

// Observed reads the sessions that started outside Trebi, and turns the hooks
// that report them on and off.
type Observed struct {
	host func(context.Context) (Host, error)
}

// NewObserved gives the observed sessions of the visor that host returns.
func NewObserved(host func(context.Context) (Host, error)) *Observed {
	return &Observed{host: host}
}

// List gives the observed sessions, the last active first.
func (o *Observed) List(ctx context.Context) ([]ObservedSession, error) {
	h, err := o.host(ctx)
	if err != nil {
		return nil, err
	}
	ss, err := h.Sessions(ctx, agentvisor.Filter{Kind: agentvisor.KindObserved})
	if err != nil {
		return nil, fmt.Errorf("list the observed sessions: %w", err)
	}
	out := make([]ObservedSession, 0, len(ss))
	for _, s := range ss {
		out = append(out, observedOf(s))
	}
	slices.SortFunc(out, func(a, b ObservedSession) int { return b.LastActivity.Compare(a.LastActivity) })
	return out, nil
}

// Get gives one observed session and its transcript.
func (o *Observed) Get(ctx context.Context, id string) (ObservedSession, []ObservedTurn, error) {
	h, err := o.host(ctx)
	if err != nil {
		return ObservedSession{}, nil, err
	}
	s, err := h.Session(ctx, id)
	if err == nil && s.Kind != agentvisor.KindObserved {
		err = agentvisor.ErrNotFound
	}
	if err != nil {
		return ObservedSession{}, nil, fmt.Errorf("session %s: %w", id, err)
	}
	turns, err := o.turns(ctx, h, s)
	return observedOf(s), turns, err
}

func turnOf(e agentvisor.Event) (ObservedTurn, bool) {
	t := ObservedTurn{At: e.At}
	switch {
	case e.Type == agentvisor.EventUser && e.Text != "":
		t.Role, t.Text = "user", e.Text
	case e.Type == agentvisor.EventResult && e.Result != nil && e.Result.Text != "":
		t.Role, t.Text = "assistant", e.Result.Text
		if e.Result.IsError {
			t.Role = "error"
		}
	case e.Type == agentvisor.EventRequest && e.Request != nil:
		t.Role, t.Text = "request", e.Request.Question
		if t.Text == "" && e.Request.Tool != "" {
			t.Text = "Wants to use " + e.Request.Tool + "."
		}
	case e.Type == agentvisor.EventError && e.Error != "":
		t.Role, t.Text = "error", e.Error
	default:
		return t, false
	}
	return t, true
}

func observedOf(s agentvisor.Session) ObservedSession {
	return ObservedSession{
		ID: s.ID, Harness: s.Harness, Folder: s.WorkingDir, State: string(s.State), Turns: s.Turns,
		Waiting: len(s.Pending), LastActivity: s.UpdatedAt, StartedAt: s.CreatedAt,
	}
}

// Hooks gives the hook state of a harness in the user login: on, off,
// outdated, untrusted, or unsupported.
func (o *Observed) Hooks(ctx context.Context, harness string) (string, error) {
	return o.hooks(ctx, harness, hookHost.Hooks)
}

// SetHooks turns the hooks of a harness on or off in the user login, and
// gives the new state.
func (o *Observed) SetHooks(ctx context.Context, harness string, on bool) (string, error) {
	if on {
		return o.hooks(ctx, harness, hookHost.InstallHooks)
	}
	return o.hooks(ctx, harness, hookHost.RemoveHooks)
}

// turns reads the events of s up to its last one.
func (o *Observed) turns(ctx context.Context, h Host, s agentvisor.Session) ([]ObservedTurn, error) {
	out := []ObservedTurn{}
	if s.Seq == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := h.Events(ctx, agentvisor.Filter{Session: s.ID}, 0)
	if err != nil {
		return nil, fmt.Errorf("read session %s: %w", s.ID, err)
	}
	defer st.Close() //nolint:errcheck // Close always gives nil
	for {
		e, err := st.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("read session %s: %w", s.ID, err)
		}
		if t, ok := turnOf(e); ok {
			out = append(out, t)
		}
		if e.Seq >= s.Seq {
			return out, nil
		}
	}
}

func (o *Observed) hooks(ctx context.Context, harness string, call func(hookHost, context.Context, agentvisor.HookSpec) ([]agentvisor.HookStatus, error)) (string, error) {
	h, err := o.host(ctx)
	if err != nil {
		return "", err
	}
	hh, ok := h.(hookHost)
	if !ok {
		return "", fmt.Errorf("hooks of %s: %w", harness, ErrNoHooks)
	}
	// Sessions outside Trebi use the login of the user, not the one of Trebi.
	list, err := call(hh, ctx, agentvisor.HookSpec{Harnesses: []string{harness}, Auth: agentvisor.AuthShared})
	if errors.Is(err, agentvisor.ErrUnsupported) {
		return HooksUnsupported, nil
	}
	if err != nil {
		return "", fmt.Errorf("hooks of %s: %w", harness, err)
	}
	state := HooksOff
	for _, s := range list {
		switch s.State {
		case agentvisor.HookState("installed"):
			state = HooksOn
		case agentvisor.HookState("outdated"):
			return HooksOutdated, nil
		case agentvisor.HookState("untrusted"):
			return HooksUntrusted, nil
		}
	}
	return state, nil
}
