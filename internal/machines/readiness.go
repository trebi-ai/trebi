package machines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/secrets"
)

// The kinds of a readiness item.
const (
	KindHarness   = "harness"
	KindConnector = "connector"
	KindSecret    = "secret"
)

// Item is one thing that a job or an agent needs on its machine. Action is
// the verb that fixes it; "" when the item is ready or no verb applies.
type Item struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Action string `json:"action"`
}

// Needs is what one job or agent needs to run. An empty Harness is the
// default harness of the config.
type Needs struct {
	Harness    string
	Connectors []string
	Secrets    []string
}

// NeedsOf reads the needs of a job with its kit merged in.
func NeedsOf(j *job.Job, kit *job.KitYAML) Needs {
	n := Needs{Harness: j.Harness, Secrets: j.SecretRefs(kit)}
	var kitList []string
	if kit != nil {
		kitList = kit.Connectors
	}
	for _, c := range slices.Concat(kitList, j.Connectors) {
		if c != "" && c != job.EmailConnector && !slices.Contains(n.Connectors, c) {
			n.Connectors = append(n.Connectors, c)
		}
	}
	return n
}

// Readiness checks the needs on the machine of the workspace: the harness
// first, then the connectors, then the secrets. A workspace on another
// machine gives unknown items, because this machine cannot check them.
func (s *Service) Readiness(ctx context.Context, workspaceID string, n Needs) ([]Item, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if n.Harness == "" {
		if f := strings.Fields(cfg.Defaults.Command); len(f) > 0 {
			n.Harness = f[0]
		}
	}
	local, err := s.onSelf(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !local {
		out := []Item{{Kind: KindHarness, Name: n.Harness, State: StateUnknown}}
		for _, c := range n.Connectors {
			out = append(out, Item{Kind: KindConnector, Name: c, State: StateUnknown})
		}
		for _, sec := range n.Secrets {
			out = append(out, Item{Kind: KindSecret, Name: sec, State: StateUnknown})
		}
		return out, nil
	}
	h, err := s.harnessItem(ctx, cfg, n.Harness)
	if err != nil {
		return nil, err
	}
	out := []Item{h}
	need := slices.Clone(n.Secrets)
	if len(n.Connectors) > 0 && s.conns != nil {
		rows := s.conns.List(workspaceID)
		for _, c := range n.Connectors {
			state := StateReady
			res, rerr := s.conns.Resolve(c, workspaceID)
			if rerr == nil {
				need = append(need, secretRefs(res)...)
				state = connectorState(nil, loginState(rowByID(rows, res.ID)))
			} else {
				state = connectorState(rerr, "")
			}
			out = append(out, newItem(KindConnector, c, state))
		}
	}
	have, err := secrets.List()
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	slices.Sort(need)
	for _, sec := range slices.Compact(need) {
		state := StateMissing
		if slices.Contains(have, sec) {
			state = StateReady
		}
		out = append(out, newItem(KindSecret, sec, state))
	}
	return out, nil
}

func (s *Service) harnessItem(ctx context.Context, cfg *config.Config, name string) (Item, error) {
	var states map[string]harness.State
	if s.auth != nil {
		var err error
		if states, err = s.auth.States(ctx); err != nil {
			return Item{}, fmt.Errorf("read harness logins: %w", err)
		}
	}
	for _, info := range harness.List(cfg, s.loc) {
		if info.Name == name {
			st, ok := states[name]
			return newItem(KindHarness, name, harnessState(info, st, ok)), nil
		}
	}
	return newItem(KindHarness, name, StateMissing), nil
}

// onSelf reports whether the workspace is on this machine. A workspace with
// no machine is on this machine.
func (s *Service) onSelf(ctx context.Context, workspaceID string) (bool, error) {
	if workspaceID == "" {
		return true, nil
	}
	var self sql.NullInt64
	err := s.st.DB().QueryRowContext(ctx, `SELECT m.is_self FROM workspaces w
		LEFT JOIN machines m ON m.id = w.machine_id WHERE w.id=?`, workspaceID).Scan(&self)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read workspace machine: %w", err)
	}
	return !self.Valid || self.Int64 == 1, nil
}

// secretRefs lists the secret names that the inputs of a connection point
// to. It reads no secret value.
func secretRefs(r connectors.Resolved) []string {
	env := map[string]string{}
	if r.Manifest.Setup != nil {
		for _, in := range r.Manifest.Setup.Inputs {
			env[in.Name] = in.Default
		}
	}
	maps.Copy(env, r.Inputs)
	var out []string
	for _, v := range env {
		if name, ok := strings.CutPrefix(v, "secret:"); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

func rowByID(rows []map[string]any, id string) map[string]any {
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	return nil
}

func newItem(kind, name, state string) Item {
	return Item{Kind: kind, Name: name, State: state, Action: action(kind, state)}
}

// action is the user verb that fixes an item in a state.
func action(kind, state string) string {
	switch state {
	case StateNotInstalled:
		return "Install"
	case StateOff:
		return "Turn on"
	case StateLoggedOut:
		return "Log in"
	case StateUnknown:
		if kind == KindHarness {
			return "Log in"
		}
	case StateError:
		return "Set up"
	case StateMissing:
		switch kind {
		case KindConnector:
			return "Connect"
		case KindSecret:
			return "Add secret"
		}
	}
	return ""
}
