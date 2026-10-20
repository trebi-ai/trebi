package workspaces

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/skills"
)

var _ skills.Owners = (*Service)(nil)

// SkillOwner returns the folder that holds the skills of one workspace,
// agent, or job: the workspace root, the AGENT.md folder, or the JOB.md
// folder.
func (s *Service) SkillOwner(ctx context.Context, scope skills.Scope, id string) (skills.Owner, error) {
	switch scope {
	case skills.ScopeWorkspace:
		ws, err := s.Get(ctx, id)
		if err != nil {
			return skills.Owner{}, err
		}
		return skills.Owner{Kind: scope, WorkspaceID: ws.ID, ID: ws.ID, Name: ws.Name, Dir: ws.Path}, nil
	case skills.ScopeAgent, skills.ScopeJob:
		row, err := s.JobByID(ctx, id)
		if err != nil {
			return skills.Owner{}, err
		}
		o, ok := jobSkillOwner(*row)
		if !ok || o.Kind != scope {
			return skills.Owner{}, fmt.Errorf("%s %s: %w", scope, id, sql.ErrNoRows)
		}
		return o, nil
	}
	return skills.Owner{}, fmt.Errorf("scope %q has no owner", scope)
}

// SkillOwners lists every workspace, agent, and job that can hold skills.
func (s *Service) SkillOwners(ctx context.Context) ([]skills.Owner, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []skills.Owner
	for _, ws := range list {
		out = append(out, skills.Owner{Kind: skills.ScopeWorkspace, WorkspaceID: ws.ID, ID: ws.ID, Name: ws.Name, Dir: ws.Path})
	}
	rows, err := s.ListJobs(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if o, ok := jobSkillOwner(row); ok {
			out = append(out, o)
		}
	}
	return out, nil
}

// jobSkillOwner maps a live job or agent row to its skill owner.
func jobSkillOwner(row JobRow) (skills.Owner, bool) {
	if row.RetiredAt != "" {
		return skills.Owner{}, false
	}
	o := skills.Owner{WorkspaceID: row.WorkspaceID, ID: row.ID, Name: row.Name, Dir: filepath.Dir(row.Path)}
	switch row.Kind {
	case "agent":
		o.Kind = skills.ScopeAgent
	case "job":
		o.Kind = skills.ScopeJob
		o.AgentID = row.OwnerID
	default:
		return skills.Owner{}, false
	}
	return o, true
}
