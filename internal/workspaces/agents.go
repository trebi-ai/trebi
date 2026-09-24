package workspaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// RenameAgent moves agents/<from> to agents/<to> and rewrites stored
// references in one transaction. The index follows the move without emitting
// synthetic deleted/created pairs for the renamed subtree.
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
	if _, ok := existing[fromIndex]; !ok {
		return ErrNotFound
	}
	if _, ok := existing[toIndex]; ok {
		return ErrAgentExists
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "agents", to)); err == nil {
		return ErrAgentExists
	} else if !os.IsNotExist(err) {
		return err
	}

	var affected []string
	for name := range existing {
		if name == fromIndex || strings.HasPrefix(name, fromIndex+"/") {
			affected = append(affected, name)
		}
	}
	if live, err := hasLiveRuns(ctx, s.st, workspaceID, affected); err != nil {
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
	if original, readErr := os.ReadFile(agentPath); readErr == nil {
		parsed, _ := job.ParseBytes(agentPath, original)
		if parsed != nil && parsed.ParseError == "" {
			data, err := os.ReadFile(agentPath)
			if err == nil {
				// Only rewrite when a scalar name key exists. job.SetName adds
				// the key otherwise, which is a visible but harmless identity
				// assertion; preserve the user's shape by checking first.
				if hasFrontmatterName(data) {
					if err := job.SetName(agentPath, to); err != nil {
						restore()
						return err
					}
				}
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
	desired := map[string]indexRow{}
	for _, j := range disc.Jobs {
		desired[j.Name] = rowFromJob(j, "job")
	}
	if disc.Assistant != nil {
		desired[disc.Assistant.Name] = rowFromJob(disc.Assistant, "assistant")
	}
	for _, j := range disc.Agents {
		desired[j.Name] = rowFromJob(j, "agent")
	}

	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if err := renameJobReferencesTx(ctx, tx, workspaceID, fromIndex, toIndex, from, to); err != nil {
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
		Topic: "agent.renamed", Entity: "agent", EntityID: toIndex, Action: "renamed",
		Attrs: map[string]string{"workspace": workspaceID, "from": fromIndex, "to": toIndex},
	}); err != nil {
		s.log.Warn("agent rename bus append failed", "workspace", workspaceID, "error", err)
	}
	return nil
}

func renameJobReferencesTx(ctx context.Context, tx *sql.Tx, workspaceID, fromIndex, toIndex, fromShort, toShort string) error {
	statements := []struct {
		query  string
		fields []any
	}{
		{`UPDATE runs SET job = ? || substr(job, length(?) + 1)
			WHERE workspace_id = ? AND (job = ? OR job LIKE ?)`,
			[]any{toIndex, fromIndex, workspaceID, fromIndex, fromIndex + "/%"}},
		{`UPDATE event_queue SET job = ? || substr(job, length(?) + 1)
			WHERE workspace_id = ? AND (job = ? OR job LIKE ?)`,
			[]any{toIndex, fromIndex, workspaceID, fromIndex, fromIndex + "/%"}},
		{`UPDATE schedules SET job = ? || substr(job, length(?) + 1)
			WHERE workspace_id = ? AND (job = ? OR job LIKE ?)`,
			[]any{toIndex, fromIndex, workspaceID, fromIndex, fromIndex + "/%"}},
		{`UPDATE email_messages SET agent = ?
			WHERE workspace_id = ? AND agent = ?`,
			[]any{toShort, workspaceID, fromShort}},
		{`UPDATE conversations SET operator = ?
			WHERE workspace_id = ? AND operator = ?`,
			[]any{toIndex, workspaceID, fromIndex}},
		{`UPDATE inbox_items SET job = ?
			WHERE workspace_id = ? AND job = ?`,
			[]any{toIndex, workspaceID, fromIndex}},
		{`UPDATE event_outcomes SET job = ?
			WHERE workspace_id = ? AND job = ?`,
			[]any{toIndex, workspaceID, fromIndex}},
		{`UPDATE agent_proposals SET status = 'stale', revision = revision + 1
			WHERE workspace_id = ? AND agent = ? AND status = 'pending'`,
			[]any{workspaceID, fromIndex}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.fields...); err != nil {
			return err
		}
	}
	return nil
}

func hasLiveRuns(ctx context.Context, st *store.Store, workspaceID string, jobs []string) (bool, error) {
	if len(jobs) == 0 {
		return false, nil
	}
	conds := make([]string, 0, len(jobs))
	args := make([]any, 0, len(jobs)+2)
	args = append(args, workspaceID)
	for _, job := range jobs {
		conds = append(conds, `(job = ? OR job LIKE ?)`)
		args = append(args, job, job+"/%")
	}
	args = append(args, "running")
	q := `SELECT EXISTS(SELECT 1 FROM runs WHERE workspace_id = ? AND (` +
		strings.Join(conds, " OR ") + `) AND status = ?)`
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
