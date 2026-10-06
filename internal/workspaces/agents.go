package workspaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

var (
	// ErrAgentExists is returned when a rename target is already an agent.
	ErrAgentExists = errors.New("agent exists")
	// ErrAgentLiveRun is returned when a rename would strand a live run.
	ErrAgentLiveRun = errors.New("agent has a live run")
)

// RenameAgent moves agents/<from> to agents/<to>. The files keep their
// IDs, so the index rows keep their IDs and every reference stays valid.
// The index follows the move without deleted/created pairs.
func (s *Service) RenameAgent(ctx context.Context, workspaceID, from, to string) error {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return fmt.Errorf("from and to are required")
	}
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return err
	}
	workspaceID = ws.ID
	fromIndex, toIndex := "agents/"+from, "agents/"+to

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.existingIndexRows(ctx, workspaceID)
	if err != nil {
		return err
	}
	agentID := ""
	var affected []string
	for id, row := range existing {
		switch {
		case row.name == toIndex:
			return ErrAgentExists
		case row.name == fromIndex:
			agentID = id
			affected = append(affected, id)
		case strings.HasPrefix(row.name, fromIndex+"/"):
			affected = append(affected, id)
		}
	}
	if agentID == "" {
		return ErrNotFound
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "agents", to)); err == nil {
		return ErrAgentExists
	} else if !os.IsNotExist(err) {
		return err
	}
	if live, err := hasLiveRuns(ctx, s.st, affected); err != nil {
		return err
	} else if live {
		return ErrAgentLiveRun
	}

	oldPath := filepath.Join(ws.Path, "agents", from)
	newPath := filepath.Join(ws.Path, "agents", to)
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	restore := func() {
		_ = os.Rename(newPath, oldPath)
	}

	// Preserve an explicit identity `name:` by rewriting it with the folder.
	agentPath := filepath.Join(newPath, "AGENT.md")
	if data, readErr := os.ReadFile(agentPath); readErr == nil {
		if parsed, _ := job.ParseBytes(agentPath, data); parsed != nil && parsed.ParseError == "" && hasFrontmatterName(data) {
			if err := job.SetName(agentPath, to); err != nil {
				restore()
				return err
			}
		}
	}

	// Discover after the filesystem move so child owners and hashes reflect
	// the on-disk truth. Existing supplies the precise diff baseline.
	otherRoots, err := s.otherWorkspaceRoots(ctx, workspaceID)
	if err != nil {
		restore()
		return err
	}
	disc, err := job.Discover(ws.Path, otherRoots...)
	if err != nil {
		restore()
		return err
	}
	jobs := append(slices.Clone(disc.Jobs), disc.Agents...)
	if err := s.ensureIDs(ctx, workspaceID, jobs, existing); err != nil {
		restore()
		return err
	}
	desired := desiredRows(jobs, nil)
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_proposals SET status = 'stale', revision = revision + 1
			WHERE agent_id = ? AND status = 'pending'`, agentID); err != nil {
			return err
		}
		return writeIndexDiffTx(ctx, tx, workspaceID, existing, desired)
	})
	if err != nil {
		restore()
		return err
	}
	// Explicitly invalidate: the fingerprint alone can remain unchanged for
	// a pure move with identical content.
	delete(s.fp, workspaceID)
	delete(s.jobs, workspaceID)

	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "agent.renamed", Entity: "agent", EntityID: agentID, Action: "renamed",
		Attrs: map[string]string{"workspace": workspaceID, "agent_id": agentID, "agent_name": to, "from": from, "to": to},
	}); err != nil {
		s.log.Warn("agent rename bus append failed", "workspace", workspaceID, "error", err)
	}
	return nil
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
