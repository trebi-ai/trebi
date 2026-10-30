package runs

import (
	"context"
	"database/sql"
	"fmt"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/visor"
)

// Labels that trebi puts on an agent-visor session.
const (
	LabelRun       = "trebi.run"
	LabelJob       = "trebi.job"
	LabelWorkspace = "trebi.workspace"
)

// SetSessionHost sets the agent-visor that runs the cli sessions. The daemon
// sets the visor service. With no host, the first launch opens a visor in
// this process (tests).
func (s *Service) SetSessionHost(h visor.Host) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = h
}

// SessionHost gives the visor, and opens one in this process when none is
// set. The assistant uses the same visor.
func (s *Service) SessionHost(ctx context.Context) (visor.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host != nil {
		return s.host, nil
	}
	var drivers map[string]agentwire.Driver
	if s.cliDriver != nil {
		drivers = map[string]agentwire.Driver{}
		for _, h := range agentwire.Harnesses() {
			drivers[string(h)] = s.cliDriver
		}
	}
	v, err := agentvisor.New(context.WithoutCancel(ctx), agentvisor.Options{
		Home: home.VisorDir(), Client: "trebi", ClientVersion: apiVersion, Drivers: drivers, Logger: s.log,
	})
	if err != nil {
		return nil, fmt.Errorf("open the session host: %w", err)
	}
	s.host, s.ownHost = v, v
	return v, nil
}

// closeOwnHost closes the visor that this process opened.
func (s *Service) closeOwnHost(ctx context.Context) {
	s.mu.Lock()
	v := s.ownHost
	if v != nil {
		s.host, s.ownHost = nil, nil
	}
	s.mu.Unlock()
	if v != nil {
		if err := v.Close(ctx); err != nil {
			s.log.Warn("close the session host", "err", err)
		}
	}
}

// setRunVisor records the visor session of a run and the last event that the
// run handled.
func (s *Service) setRunVisor(ctx context.Context, runID, sessionID string, seq uint64) {
	if _, err := s.st.DB().ExecContext(ctx, `UPDATE runs SET visor_session_id=?, visor_seq=? WHERE id=?`, sessionID, int64(seq), runID); err != nil { //nolint:gosec // a seq stays far below 2^63
		s.log.Warn("save the visor session of a run", "run_id", runID, "err", err)
	}
}

// setRunVisorSeq records the last visor event that the run handled.
func (s *Service) setRunVisorSeq(ctx context.Context, runID string, seq uint64) {
	if _, err := s.st.DB().ExecContext(ctx, `UPDATE runs SET visor_seq=? WHERE id=?`, int64(seq), runID); err != nil { //nolint:gosec // a seq stays far below 2^63
		s.log.Warn("save the visor seq of a run", "run_id", runID, "err", err)
	}
}

// runVisor gives the visor session of a run and the last event that the run
// handled.
func (s *Service) runVisor(ctx context.Context, runID string) (string, uint64, error) {
	var id sql.NullString
	var seq int64
	err := s.st.DB().QueryRowContext(ctx, `SELECT visor_session_id, visor_seq FROM runs WHERE id=?`, runID).Scan(&id, &seq)
	return id.String, uint64(seq), err //nolint:gosec // the column holds a seq, never negative
}

// startVisorSession starts the session of a cli run turn and follows its
// events.
func (s *Service) startVisorSession(ctx context.Context, run *Run, req agentwire.StartRequest, resuming bool) (*visor.Session, error) {
	host, err := s.SessionHost(ctx)
	if err != nil {
		return nil, err
	}
	spec := agentvisor.StartSpec{
		Kind: agentvisor.KindWire, Harness: string(req.Harness), Binary: req.Binary, WorkingDir: req.WorkingDir, Env: req.Env,
		Model: req.Model, Effort: req.Effort, ExtraArgs: req.ExtraArgs, Raw: req.Raw,
		Permissions: agentvisor.Permissions{Mode: req.Permissions.Mode, AllowTools: req.Permissions.AllowTools},
		Labels:      map[string]string{LabelRun: run.ID, LabelJob: run.JobID, LabelWorkspace: run.Workspace},
	}
	if resuming {
		spec.ResumeID = req.SessionID
	}
	for _, m := range req.MCPServers {
		spec.MCPServers = append(spec.MCPServers, agentvisor.MCPServer{Name: m.Name, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers})
	}
	for _, sk := range req.Skills {
		spec.Skills = append(spec.Skills, agentvisor.Skill{Name: sk.Name, Dir: sk.Dir})
	}
	vs, err := visor.Start(ctx, host, spec)
	if err != nil {
		return nil, err
	}
	s.setRunVisor(ctx, run.ID, vs.ID(), 0)
	return vs, nil
}
