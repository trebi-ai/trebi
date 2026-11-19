package workspaces

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/store"
)

// RouteRow is one routes file of one live workspace.
type RouteRow struct {
	WorkspaceID string
	Workspace   string
	File        routing.File
}

// LocatedRoute is a route and the connection whose file holds it.
type LocatedRoute struct {
	Connection string
	Route      routing.Route
}

// RouteRows reads the routes files of each workspace that is not orphaned
// and not archived. A file that does not parse gives a row with problems.
func (s *Service) RouteRows(ctx context.Context) ([]RouteRow, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []RouteRow
	for i := range list {
		ws := &list[i]
		if ws.Orphaned || ws.ArchivedAt != nil {
			continue
		}
		files, err := routing.LoadDir(ws.Path)
		if err != nil {
			s.log.Warn("routes read failed", "workspace", ws.ID, "error", err)
			continue
		}
		for _, f := range files {
			out = append(out, RouteRow{WorkspaceID: ws.ID, Workspace: ws.Name, File: f})
		}
	}
	return out, nil
}

// Routing loads the routes file of one connection in one workspace.
func (s *Service) Routing(ctx context.Context, workspaceID, connection string) (routing.File, error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return routing.File{}, notFound(err)
	}
	return routing.Load(ws.Path, connection)
}

// RoutingFiles loads each routes file of one workspace.
func (s *Service) RoutingFiles(ctx context.Context, workspaceID string) ([]routing.File, error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return nil, notFound(err)
	}
	return routing.LoadDir(ws.Path)
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// RouteTargets lists the agents, the jobs of agents, and the assistant that
// a route of the workspace can name, keyed by short name. A job outside an
// agent is no target.
func (s *Service) RouteTargets(ctx context.Context, workspaceID string) (map[string]routing.Target, error) {
	rows, err := s.ListJobs(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]routing.Target{
		routing.Assistant: {Kind: routing.Assistant, Name: routing.Assistant, Index: routing.Assistant, ID: workspaceID, Active: true},
	}
	kits := map[string][]string{} // agent id -> kit connectors
	parsed := map[string]*job.Job{}
	for _, row := range rows {
		if j, _ := job.ParseFile(row.Path); j != nil {
			parsed[row.ID] = j
			if row.Kind == "agent" {
				if kit := j.OwnerKit(); kit != nil {
					kits[row.ID] = kit.Connectors
				}
			}
		}
	}
	for _, row := range rows {
		if row.Kind == "job" && row.OwnerID == "" {
			continue
		}
		t := routing.Target{
			Kind: row.Kind, Name: strings.TrimPrefix(row.Name, "agents/"), Index: row.Name,
			ID: row.ID, Active: row.Active, Archived: row.Archived(), Kit: kits[row.AgentID()],
		}
		if j := parsed[row.ID]; j != nil {
			t.Job, t.Description = j, j.Description
		}
		out[t.Name] = t
	}
	return out, nil
}

// RoutesOf lists the routes of the workspace that name target, a short
// name or an index name.
func (s *Service) RoutesOf(ctx context.Context, workspaceID, target string) ([]LocatedRoute, error) {
	files, err := s.RoutingFiles(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	target = strings.TrimPrefix(target, "agents/")
	var out []LocatedRoute
	for _, f := range files {
		for _, r := range f.Routes {
			if !r.Broken() && r.TargetName() == target {
				out = append(out, LocatedRoute{Connection: f.Connection, Route: r})
			}
		}
	}
	return out, nil
}

// EnsureRouteIDs writes an id into each route that has none, and emits
// routing.updated for each file it changes. A failed write is logged.
func (s *Service) EnsureRouteIDs(ctx context.Context, ws *Workspace) {
	dir := filepath.Join(ws.Path, routing.Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if conn, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() && s.assignFileIDs(filepath.Join(dir, e.Name())) {
			s.emitRouting(ctx, ws.ID, conn)
		}
	}
}

// RoutesFileChanged handles an edit of one routes file on disk: it writes
// the missing route ids and emits routing.updated once.
func (s *Service) RoutesFileChanged(ctx context.Context, ws *Workspace, path string) {
	conn, ok := strings.CutSuffix(filepath.Base(path), ".yaml")
	if !ok {
		return
	}
	s.assignFileIDs(path)
	s.emitRouting(ctx, ws.ID, conn)
}

// assignFileIDs reports whether it wrote ids into the file.
func (s *Service) assignFileIDs(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	out, changed, err := routing.AssignIDs(data)
	if err != nil || !changed {
		return false
	}
	if err := job.WriteAtomic(path, out); err != nil {
		s.log.Warn("route id write failed", "path", path, "error", err)
		return false
	}
	return true
}

func (s *Service) emitRouting(ctx context.Context, wsID, conn string) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "routing.updated", Entity: "workspace", EntityID: wsID, Action: "updated",
		Attrs: map[string]string{"workspace": wsID, "connection": conn},
	}); err != nil {
		s.log.Warn("routing bus append failed", "workspace", wsID, "error", err)
	}
}

// routeSenders lists the allowlist entries of the routes of f that name target.
func routeSenders(f routing.File, target string) []string {
	var out []string
	for _, r := range f.Routes {
		if !r.Broken() && r.TargetName() == target {
			out = append(out, r.Filter.Allowlist...)
		}
	}
	return out
}
