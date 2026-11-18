package workspaces

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/job"
)

// AcceptRules returns the resolved inbound-mail gates. Agents read AGENT.md;
// the assistant reads the trebi.yaml override with the operators-plus-
// allowlist default (plan 2026-09-26 B.2).
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
		if row.Kind != "agent" || !row.Active || row.Archived() {
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
	ov, _, oerr := s.AssistantOverride(ctx, ws.ID)
	if oerr != nil {
		return agents, nil, oerr
	}
	accept := wire.DefaultAccept
	if ov.Email != nil && len(ov.Email.Accept) > 0 {
		accept = ov.Email.Accept
	}
	assistant, perr := job.ParseAccept(accept)
	if perr != nil {
		return agents, nil, perr
	}
	return agents, assistant, nil
}

// JobEmailSenders returns the sender addresses of the event: email triggers
// of one job. The job send gate accepts them (TREBI-510): an empty list
// means every sender for the inbound gate, so it opens no send address.
func (s *Service) JobEmailSenders(ctx context.Context, workspaceID, name string) ([]string, error) {
	row, err := s.JobByName(ctx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	j, err := job.ParseFile(row.Path)
	if err != nil || j == nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, tr := range j.Triggers {
		if tr.Family != "event" || tr.Event != "email" {
			continue
		}
		for _, entry := range tr.Allowlist {
			entry = strings.ToLower(strings.TrimSpace(entry))
			if entry == "" || !strings.Contains(entry, "@") || seen[entry] {
				continue
			}
			seen[entry] = true
			out = append(out, entry)
		}
	}
	sort.Strings(out)
	return out, nil
}

// OwnerKit returns the owning agent's reusable kit for a job or operator
// definition, with the agent working_dir. An owner agent row that is gone
// or does not parse returns nil.
func (s *Service) OwnerKit(ctx context.Context, workspaceID, name string) *job.KitYAML {
	rows, err := s.ListJobs(ctx, workspaceID)
	if err != nil {
		return nil
	}
	owner := name
	if row, err := s.findRow(rows, name); err == nil {
		owner = row.OwnerIndex()
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
				return parsed.OwnerKit()
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

// EmailStyle returns the mail style of one sender. The trebi.yaml assistant
// block sets the workspace style; an agent's AGENT.md overrides each field.
func (s *Service) EmailStyle(ctx context.Context, workspaceID, agent string) (job.StyleYAML, error) {
	ov, _, err := s.AssistantOverride(ctx, workspaceID)
	if err != nil {
		return job.StyleYAML{}, err
	}
	var style job.StyleYAML
	if ov.Email != nil && ov.Email.Style != nil {
		if err := ov.Email.Style.Validate(); err != nil {
			return job.StyleYAML{}, fmt.Errorf("trebi.yaml assistant: %w", err)
		}
		style = *ov.Email.Style
	}
	if agent == "" {
		return style, nil
	}
	rows, err := s.ListJobs(ctx, workspaceID)
	if err != nil {
		return style, err
	}
	for _, row := range rows {
		if row.Kind == "agent" && trimAgentPrefix(row.Name) == agent {
			if j, _ := job.ParseFile(row.Path); j != nil {
				return j.EmailStyle.Merge(style), nil
			}
		}
	}
	return style, nil
}
