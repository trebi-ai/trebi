package workspaces

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
)

// AcceptRules returns the resolved inbound-mail gates from the files. Mail
// volume is low and the files are the source of truth, so there is no cache.
func (s *Service) AcceptRules(ctx context.Context, workspaceID string) (agents map[string][]job.AcceptEntry, assistant []job.AcceptEntry, err error) {
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.ListJobs(ctx, ws.ID)
	if err != nil {
		return nil, nil, err
	}
	agents = make(map[string][]job.AcceptEntry)
	for _, row := range rows {
		if row.Kind != "agent" || !row.Active {
			continue
		}
		j, perr := job.ParseFile(row.Path)
		if j == nil {
			if perr != nil {
				s.log.Warn("agent accept parse failed", "path", row.Path, "error", perr.Error())
			}
			continue
		}
		entries, perr := job.ParseAccept(j.EmailAccept)
		if perr != nil {
			s.log.Warn("agent accept invalid", "path", row.Path, "error", perr.Error())
			continue
		}
		agents[trimAgentPrefix(row.Name)] = entries
	}
	assistantPath := filepath.Join(ws.Path, "TREBI.md")
	assistant = job.DefaultAssistantAccept
	if j, perr := job.ParseFile(assistantPath); j != nil && perr == nil {
		if j.Accept != nil {
			assistant = j.Accept
		} else if entries, perr := job.ParseAccept(j.EmailAccept); perr == nil && len(entries) > 0 {
			assistant = entries
		}
	}
	return agents, assistant, nil
}

// OwnerKit returns the owning agent's reusable kit for a job or operator
// definition. A missing agent or parse failure returns nil.
func (s *Service) OwnerKit(ctx context.Context, workspaceID, name string) *job.KitYAML {
	rows, err := s.ListJobs(ctx, workspaceID)
	if err != nil {
		return nil
	}
	owner := name
	if row, err := s.findRow(rows, name); err == nil {
		owner = row.Owner
		if owner == "" && row.Kind == "agent" {
			owner = row.Name
		}
	}
	if owner == "" {
		return nil
	}
	for _, row := range rows {
		if row.Kind == "agent" && row.Name == owner {
			if parsed, err := job.ParseFile(row.Path); err == nil && parsed != nil {
				return parsed.Kit
			}
		}
	}
	return nil
}

func (s *Service) findRow(rows []JobRow, name string) (JobRow, error) {
	for _, row := range rows {
		if row.Name == name || strings.TrimPrefix(row.Name, "agents/") == name {
			return row, nil
		}
	}
	return JobRow{}, ErrNotFound
}

func trimAgentPrefix(name string) string {
	const prefix = "agents/"
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}
