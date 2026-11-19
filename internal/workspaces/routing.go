package workspaces

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
// a route of the workspace can name, keyed by index name. A job outside an
// agent is no target.
func (s *Service) RouteTargets(ctx context.Context, workspaceID string) (map[string]routing.Target, error) {
	rows, err := s.ListJobs(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]routing.Target{
		routing.Assistant: {Kind: routing.Assistant, Name: routing.Assistant, ID: workspaceID, Active: true},
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
			Kind: row.Kind, Name: row.Name,
			ID: row.ID, Active: row.Active, Archived: row.Archived(), Kit: kits[row.AgentID()],
		}
		if j := parsed[row.ID]; j != nil {
			t.Job, t.Description = j, j.Description
		}
		out[t.Name] = t
	}
	return out, nil
}

// RoutesOf lists the routes of the workspace that name target.
func (s *Service) RoutesOf(ctx context.Context, workspaceID, target string) ([]LocatedRoute, error) {
	files, err := s.RoutingFiles(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
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
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	dir := filepath.Join(ws.Path, routing.Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		conn, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		if hash, changed := s.assignFileIDs(filepath.Join(dir, e.Name())); changed {
			s.emitRouting(ctx, ws.ID, conn, hash)
		}
	}
}

// RoutesFileChanged handles an edit of one routes file on disk: it writes
// the missing route ids and emits routing.updated once. A write of the API
// emitted already, so its echo emits nothing.
func (s *Service) RoutesFileChanged(ctx context.Context, ws *Workspace, path string) {
	conn, ok := strings.CutSuffix(filepath.Base(path), ".yaml")
	if !ok {
		return
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	hash, _ := s.assignFileIDs(path)
	if hash != "" && s.routeWrites[path] == hash {
		delete(s.routeWrites, path)
		return
	}
	s.emitRouting(ctx, ws.ID, conn, hash)
}

// assignFileIDs writes ids into the file. It returns the hash of the file
// after the call, and whether it wrote.
func (s *Service) assignFileIDs(path string) (hash string, changed bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	out, changed, err := routing.AssignIDs(data)
	if err != nil || !changed {
		return job.ContentHash(data), false
	}
	if err := job.WriteAtomic(path, out); err != nil {
		s.log.Warn("route id write failed", "path", path, "error", err)
		return job.ContentHash(data), false
	}
	return job.ContentHash(out), true
}

func (s *Service) emitRouting(ctx context.Context, wsID, conn, hash string) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "routing.updated", Entity: "routing", EntityID: wsID + "/" + conn, Action: "updated",
		Attrs: map[string]string{"workspace_id": wsID, "connection": conn, "hash": hash},
	}); err != nil {
		s.log.Warn("routing bus append failed", "workspace", wsID, "error", err)
	}
}

// routeEdit is one file that a multi-file write changed.
type routeEdit struct {
	conn string
	old  []byte
}

// writeRouting writes a routes file and emits routing.updated. The caller
// holds routeMu.
func (s *Service) writeRouting(ctx context.Context, ws *Workspace, conn string, data []byte) error {
	path := filepath.Join(ws.Path, routing.Dir, conn+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write routes %s: %w", conn, err)
	}
	if err := job.WriteAtomic(path, data); err != nil {
		return fmt.Errorf("write routes %s: %w", conn, err)
	}
	hash := job.ContentHash(data)
	if s.routeWrites == nil {
		s.routeWrites = map[string]string{}
	}
	s.routeWrites[path] = hash
	s.emitRouting(ctx, ws.ID, conn, hash)
	return nil
}

// undoRouting returns a function that writes the old bytes of the edited
// files back, newest first.
func (s *Service) undoRouting(ctx context.Context, ws *Workspace, done []routeEdit) func() error {
	return func() error {
		s.routeMu.Lock()
		defer s.routeMu.Unlock()
		return s.undoLocked(ctx, ws, done)
	}
}

// undoLocked is the body of undoRouting. The caller holds routeMu.
func (s *Service) undoLocked(ctx context.Context, ws *Workspace, done []routeEdit) error {
	var errs []error
	for _, e := range slices.Backward(done) {
		errs = append(errs, s.writeRouting(ctx, ws, e.conn, e.old))
	}
	return errors.Join(errs...)
}

// readRouting reads the bytes of a routes file. A missing file is nil.
func (s *Service) readRouting(ws *Workspace, conn string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(ws.Path, routing.Dir, conn+".yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// SetRouting applies a patch to the routes file of one connection and
// returns the new file. A missing file is an empty file. A patch fails only
// on an error that it adds, so a user can fix one fault at a time. A stale
// ifMatch is ErrPrecondition; "" skips the check.
func (s *Service) SetRouting(ctx context.Context, workspaceID, connection string, p routing.Patch, ifMatch string) (routing.File, error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return routing.File{}, notFound(err)
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	pl, err := s.planRouting(ctx, ws, connection, p, ifMatch)
	if err != nil {
		return pl.prev, err
	}
	if bytes.Equal(pl.out, pl.data) || (pl.data == nil && string(pl.out) == "{}\n") {
		return pl.prev, nil
	}
	if err := s.writeRouting(ctx, ws, connection, pl.out); err != nil {
		return pl.prev, err
	}
	return pl.next, nil
}

// PreviewRouting returns the file that SetRouting would write. It writes
// nothing. The errors are those of SetRouting, except ErrPrecondition.
func (s *Service) PreviewRouting(ctx context.Context, workspaceID, connection string, p routing.Patch) (routing.File, error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return routing.File{}, notFound(err)
	}
	pl, err := s.planRouting(ctx, ws, connection, p, "")
	if err != nil {
		return pl.prev, err
	}
	return pl.next, nil
}

// routingPlan is a patch applied to the bytes of a routes file.
type routingPlan struct {
	prev, next routing.File
	data, out  []byte
}

func (s *Service) planRouting(ctx context.Context, ws *Workspace, connection string, p routing.Patch, ifMatch string) (routingPlan, error) {
	var pl routingPlan
	targets, err := s.RouteTargets(ctx, ws.ID)
	if err != nil {
		return pl, err
	}
	path := filepath.Join(ws.Path, routing.Dir, connection+".yaml")
	if pl.data, err = s.readRouting(ws, connection); err != nil {
		return pl, fmt.Errorf("read routes %s: %w", connection, err)
	}
	pl.prev = routing.Parse(connection, path, pl.data)
	if ifMatch != "" && ifMatch != pl.prev.Hash {
		return pl, ErrPrecondition
	}
	pl.out, err = routing.Edit(pl.data, p)
	var ve *routing.ValidationError
	if errors.As(err, &ve) {
		return pl, ve.WithConnector(connection)
	}
	if err != nil {
		return pl, err
	}
	pl.next = routing.Parse(connection, path, pl.out)
	if added := newRouteErrors(pl.prev, pl.next, targets); len(added) > 0 {
		return pl, &routing.ValidationError{Problems: added}
	}
	return pl, nil
}

// newRouteErrors lists the error problems that next has and prev has not.
func newRouteErrors(prev, next routing.File, targets map[string]routing.Target) []routing.Problem {
	old := map[string]bool{}
	for _, p := range slices.Concat(prev.Problems, routing.Check(prev, targets)) {
		old[p.Code+"|"+p.Message] = true
	}
	var out []routing.Problem
	for _, p := range slices.Concat(next.Problems, routing.Check(next, targets)) {
		if p.Severity == job.SeverityError && !old[p.Code+"|"+p.Message] {
			out = append(out, p)
		}
	}
	return out
}

// RouteRefs lists the routes and rules of the workspace that name any of
// targets. An agent name also matches its jobs.
func (s *Service) RouteRefs(ctx context.Context, workspaceID string, targets []string) (routing.Refs, error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return routing.Refs{}, notFound(err)
	}
	s.EnsureRouteIDs(ctx, ws)
	files, err := routing.LoadDir(ws.Path)
	if err != nil {
		return routing.Refs{}, err
	}
	var out routing.Refs
	for _, f := range files {
		refs := f.RefsTo(targets)
		out.Routes = append(out.Routes, refs.Routes...)
		out.Rules = append(out.Rules, refs.Rules...)
	}
	return out, nil
}

// RemoveRouteRefs removes the listed routes and rules, writes each changed
// file, and returns a function that writes the old files back. A write error
// restores the files written before it.
func (s *Service) RemoveRouteRefs(ctx context.Context, workspaceID string, refs routing.Refs) (restore func() error, err error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return nil, notFound(err)
	}
	conns := map[string]bool{}
	for _, r := range refs.Routes {
		conns[r.Connection] = true
	}
	for _, r := range refs.Rules {
		conns[r.Connection] = true
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	var done []routeEdit
	for _, conn := range slices.Sorted(maps.Keys(conns)) {
		data, err := s.readRouting(ws, conn)
		if err == nil && data != nil {
			err = s.removeRefs(ctx, ws, conn, data, refs)
			if err == nil {
				done = append(done, routeEdit{conn: conn, old: data})
			}
		}
		if err != nil {
			_ = s.undoLocked(ctx, ws, done)
			return nil, err
		}
	}
	return s.undoRouting(ctx, ws, done), nil
}

// removeRefs removes the refs of one file and writes it. The caller holds
// routeMu.
func (s *Service) removeRefs(ctx context.Context, ws *Workspace, conn string, data []byte, refs routing.Refs) error {
	var p routing.Patch
	for _, r := range refs.Routes {
		if r.Connection == conn && r.Route != "" {
			p.RemoveRoutes = append(p.RemoveRoutes, r.Route)
		}
	}
	drop := map[int]bool{}
	for _, r := range refs.Rules {
		if r.Connection == conn {
			drop[r.Index] = true
		}
	}
	if len(drop) > 0 {
		keep := []routing.Rule{}
		for i, r := range routing.Parse(conn, "", data).Rules {
			if !drop[i] {
				keep = append(keep, r)
			}
		}
		p.Rules = &keep
	}
	out, err := routing.Edit(data, p)
	if err != nil {
		return fmt.Errorf("remove routes from %s: %w", conn, err)
	}
	if bytes.Equal(out, data) {
		return nil
	}
	return s.writeRouting(ctx, ws, conn, out)
}

// RenameRouteTarget rewrites the agent:, job:, and rule to: values that name
// from. An agent name also rewrites the jobs of that agent. It returns a
// function that writes the old files back.
func (s *Service) RenameRouteTarget(ctx context.Context, workspaceID, from, to string) (restore func() error, err error) {
	ws, err := s.getRaw(ctx, workspaceID)
	if err != nil {
		return nil, notFound(err)
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	entries, err := os.ReadDir(filepath.Join(ws.Path, routing.Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read routes folder: %w", err)
	}
	var done []routeEdit
	for _, e := range entries {
		conn, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		data, err := s.readRouting(ws, conn)
		if err != nil {
			_ = s.undoLocked(ctx, ws, done)
			return nil, fmt.Errorf("read routes %s: %w", conn, err)
		}
		out, changed, err := routing.RenameTarget(data, from, to)
		if err == nil && changed {
			err = s.writeRouting(ctx, ws, conn, out)
		}
		if err != nil {
			_ = s.undoLocked(ctx, ws, done)
			return nil, fmt.Errorf("rename %s in routes %s: %w", from, conn, err)
		}
		if changed {
			done = append(done, routeEdit{conn: conn, old: data})
		}
	}
	return s.undoRouting(ctx, ws, done), nil
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
