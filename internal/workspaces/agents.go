package workspaces

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

// RenameAgent moves the folder of agent from to the folder of agent to. The files keep their
// IDs, so the index rows keep their IDs and every reference stays valid.
// The index follows the move without deleted/created pairs. It returns the
// parsed agent and jobs at the new path, for the schedules.
func (s *Service) RenameAgent(ctx context.Context, workspaceID, from, to string) ([]*job.Job, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from and to are required")
	}
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	workspaceID = ws.ID

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.existingIndexRows(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	agentID := ""
	for id, row := range existing {
		if row.name == from && row.kind == "agent" {
			agentID = id
		}
	}
	if agentID == "" {
		return nil, ErrNotFound
	}
	moved, err := s.relocateObject(ctx, ws, existing, relocation{
		from: from, to: to,
		// Preserve an explicit identity `name:` by rewriting it with the folder.
		afterMove: func(newDir string) error {
			agentPath := filepath.Join(newDir, "AGENT.md")
			data, readErr := os.ReadFile(agentPath)
			if readErr != nil {
				return nil
			}
			if parsed, _ := job.ParseBytes(agentPath, data); parsed != nil && parsed.ParseError == "" && hasFrontmatterName(data) {
				return job.SetName(agentPath, to)
			}
			return nil
		},
		inTx: func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE agent_proposals SET status = 'stale', revision = revision + 1
				WHERE agent_id = ? AND status = 'pending'`, agentID)
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "agent.renamed", Entity: "agent", EntityID: agentID, Action: "renamed",
		Attrs: map[string]string{"workspace": workspaceID, "agent_id": agentID, "agent_name": to, "from": from, "to": to},
	}); err != nil {
		s.log.Warn("agent rename bus append failed", "workspace", workspaceID, "error", err)
	}
	return moved, nil
}

func hasLiveRuns(ctx context.Context, st *store.Store, ids []string) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, "running")
	q := `SELECT EXISTS(SELECT 1 FROM runs WHERE job_id IN (?` + strings.Repeat(",?", len(ids)-1) + `) AND status = ?)`
	var found bool
	if err := st.DB().QueryRowContext(ctx, q, args...).Scan(&found); err != nil {
		return false, err
	}
	return found, nil
}

func hasFrontmatterName(data []byte) bool {
	if len(data) < 4 || string(data[:3]) != "---" {
		return false
	}
	end := strings.Index(string(data[3:]), "\n---")
	if end < 0 {
		return false
	}
	for _, line := range strings.Split(string(data[3:3+end]), "\n") {
		if strings.HasPrefix(line, "name:") {
			return true
		}
	}
	return false
}
