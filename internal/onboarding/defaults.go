package onboarding

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/trebi-ai/trebi/internal/workspaces"
)

// DefaultWorkspaceName is the name of the folder that setup makes under the
// workspaces root.
const DefaultWorkspaceName = "Personal"

// Defaults is the result of ApplyDefaults.
type Defaults struct {
	Workspace *workspaces.Workspace `json:"workspace"`
	Harness   string                `json:"harness,omitempty"` // the harness that is on, or "" when 0 or 2+ are logged in
	Choose    bool                  `json:"choose"`            // true: the UI shows the AI screen
}

// HarnessSwitch is the harness side of the defaults.
type HarnessSwitch interface {
	// LoggedIn lists the harnesses with a working login.
	LoggedIn(ctx context.Context) ([]string, error)
	// TurnOn turns a harness on with the shared login. It is a no-op for a
	// harness that is already on.
	TurnOn(ctx context.Context, name string) error
}

// ApplyDefaults makes the defaults that need no screen: the Personal
// workspace, and the harness when exactly one is logged in. A second call
// makes nothing new.
func (s *Service) ApplyDefaults(ctx context.Context, ws *workspaces.Service, h HarnessSwitch) (Defaults, error) {
	var out Defaults
	st, err := s.Get(ctx)
	if err != nil {
		return out, err
	}
	if out.Workspace, err = s.defaultWorkspace(ctx, ws, st); err != nil {
		return out, err
	}
	names, err := h.LoggedIn(ctx)
	if err != nil {
		return out, fmt.Errorf("list logged-in harnesses: %w", err)
	}
	out.Choose = len(names) != 1
	if out.Choose {
		return out, nil
	}
	if err := h.TurnOn(ctx, names[0]); err != nil {
		return out, fmt.Errorf("turn on %s: %w", names[0], err)
	}
	out.Harness = names[0]
	return out, nil
}

// defaultWorkspace returns the stored workspace, or makes it when the state
// has none or the user deleted it.
func (s *Service) defaultWorkspace(ctx context.Context, ws *workspaces.Service, st State) (*workspaces.Workspace, error) {
	if st.WorkspaceID != "" {
		w, err := ws.Get(ctx, st.WorkspaceID)
		if err == nil {
			return w, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, workspaces.ErrNotFound) {
			return nil, err
		}
	}
	w, err := ws.AddWith(ctx, workspaces.AddOpts{Name: DefaultWorkspaceName, Create: true})
	if err != nil && w == nil {
		return nil, err
	}
	// A rebuild error leaves the workspace registered. Keep it and log.
	if err != nil {
		s.log.Warn("onboarding.workspace_index", "err", err)
	}
	if _, err := s.setWorkspace(ctx, w.ID); err != nil {
		return nil, err
	}
	return w, nil
}
