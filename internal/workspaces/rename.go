package workspaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

var (
	// ErrExists is returned when a rename target is already a job or agent.
	ErrExists = errors.New("rename target exists")
	// ErrLiveRun is returned when a rename would strand a live run.
	ErrLiveRun = errors.New("live run blocks the rename")
	// ErrInvalidName is returned when a new job name breaks the name rule.
	ErrInvalidName = errors.New("invalid job name")
	// ErrRootJob is returned for a job at the workspace root.
	ErrRootJob = errors.New("root job has the workspace name")
	// ErrNotJob is returned when the name is an agent, not a job.
	ErrNotJob = errors.New("name is an agent")
)

const jobLeafMax = 64

var jobLeafRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// JobRenamed is the reply of a job rename.
type JobRenamed struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// RenameJob changes the last segment of a job name. The job keeps its ID
// and its owner. It returns the parsed jobs that moved, for the schedules.
func (s *Service) RenameJob(ctx context.Context, workspaceID, name, to string) (JobRenamed, []*job.Job, error) {
	to = strings.TrimSpace(to)
	if len(to) > jobLeafMax || !jobLeafRe.MatchString(to) {
		return JobRenamed{}, nil, ErrInvalidName
	}
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return JobRenamed{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.existingIndexRows(ctx, ws.ID)
	if err != nil {
		return JobRenamed{}, nil, err
	}
	var row indexRow
	for _, r := range existing {
		if r.name == name || r.id == name {
			row = r
			break
		}
	}
	switch {
	case row.id == "":
		return JobRenamed{}, nil, ErrNotFound
	case row.kind == "agent":
		return JobRenamed{}, nil, ErrNotJob
	case row.kind != "job":
		return JobRenamed{}, nil, ErrNotFound
	case filepath.Clean(filepath.Dir(row.path)) == filepath.Clean(ws.Path):
		return JobRenamed{}, nil, ErrRootJob
	}
	toRel := path.Join(path.Dir(row.name), to)
	if toRel == row.name {
		return JobRenamed{}, nil, ErrExists
	}
	moved, err := s.relocateObject(ctx, ws, existing, relocation{fromRel: row.name, toRel: toRel})
	if err != nil {
		return JobRenamed{}, nil, err
	}
	out := JobRenamed{ID: row.id, From: row.name, To: toRel}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "job.renamed", Entity: "job", EntityID: row.id, Action: "renamed",
		Attrs: map[string]string{"workspace": ws.ID, "job_id": row.id, "job_name": toRel, "from": row.name, "to": toRel},
	}); err != nil {
		s.log.Warn("job rename bus append failed", "workspace", ws.ID, "error", err)
	}
	return out, moved, nil
}

// relocation is one folder move of a job or agent. The rel paths are also
// the index names. afterMove and inTx hold the agent-only steps.
type relocation struct {
	fromRel, toRel string
	afterMove      func(newDir string) error
	inTx           func(tx *sql.Tx) error
}

// relocateObject moves the folder of a job or agent and refreshes the index
// in one transaction. It rewrites the stored working folders of earlier
// runs and requests, so resume finds the new folder. The caller holds s.mu.
// It returns the parsed jobs and agents at the new path.
func (s *Service) relocateObject(ctx context.Context, ws *Workspace, existing map[string]indexRow, rl relocation) ([]*job.Job, error) {
	var affected []string
	for id, row := range existing {
		switch {
		case row.name == rl.toRel || strings.HasPrefix(row.name, rl.toRel+"/"):
			return nil, ErrExists
		case row.name == rl.fromRel || strings.HasPrefix(row.name, rl.fromRel+"/"):
			affected = append(affected, id)
		}
	}
	if len(affected) == 0 {
		return nil, ErrNotFound
	}
	oldPath := filepath.Join(ws.Path, filepath.FromSlash(rl.fromRel))
	newPath := filepath.Join(ws.Path, filepath.FromSlash(rl.toRel))
	if _, err := os.Stat(newPath); err == nil {
		return nil, ErrExists
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if live, err := hasLiveRuns(ctx, s.st, affected); err != nil {
		return nil, err
	} else if live {
		return nil, ErrLiveRun
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return nil, err
	}
	restore := func() {
		_ = os.Rename(newPath, oldPath) // best effort: the first error is the one to report
	}
	if rl.afterMove != nil {
		if err := rl.afterMove(newPath); err != nil {
			restore()
			return nil, err
		}
	}

	// Discover after the move so child owners and hashes reflect the disk.
	// Existing supplies the precise diff baseline.
	otherRoots, err := s.otherWorkspaceRoots(ctx, ws.ID)
	if err != nil {
		restore()
		return nil, err
	}
	disc, err := job.Discover(ws.Path, otherRoots...)
	if err != nil {
		restore()
		return nil, err
	}
	jobs := append(slices.Clone(disc.Jobs), disc.Agents...)
	if err := s.ensureIDs(ctx, ws.ID, jobs, existing); err != nil {
		restore()
		return nil, err
	}
	desired := desiredRows(jobs, idsByName(existing))
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if err := writeIndexDiffTx(ctx, tx, ws.ID, existing, desired); err != nil {
			return err
		}
		if err := rewriteWorkingDirs(ctx, tx, ws.ID, oldPath, newPath); err != nil {
			return err
		}
		if rl.inTx != nil {
			return rl.inTx(tx)
		}
		return nil
	})
	if err != nil {
		restore()
		return nil, err
	}
	// The fingerprint alone can stay the same for a pure move.
	delete(s.fp, ws.ID)
	delete(s.jobs, ws.ID)

	var moved []*job.Job
	for _, j := range jobs {
		if j.Name == rl.toRel || strings.HasPrefix(j.Name, rl.toRel+"/") {
			moved = append(moved, j)
		}
	}
	// Agent rows announce themselves with agent.renamed; jobs get job.updated.
	for id, want := range desired {
		if have, ok := existing[id]; ok && want.kind == "job" && have != want {
			if _, err := s.st.Append(ctx, IndexEvent(ws.ID, want.kind, "updated", id, want.name)); err != nil {
				s.log.Warn("job index bus append failed", "workspace", ws.ID, "error", err)
			}
		}
	}
	return moved, nil
}

// rewriteWorkingDirs points the stored working folders of runs and requests
// at the moved folder. substr counts characters, so the prefix length does too.
func rewriteWorkingDirs(ctx context.Context, tx *sql.Tx, wsID, oldDir, newDir string) error {
	prefix := oldDir + string(filepath.Separator)
	for _, table := range []string{"runs", "requests"} {
		q := fmt.Sprintf(`UPDATE %s SET working_dir = ? || substr(working_dir, ?)
			WHERE workspace_id = ? AND (working_dir = ? OR substr(working_dir, 1, ?) = ?)`, table)
		if _, err := tx.ExecContext(ctx, q, newDir, utf8.RuneCountInString(oldDir)+1,
			wsID, oldDir, utf8.RuneCountInString(prefix), prefix); err != nil {
			return fmt.Errorf("rewrite %s working_dir: %w", table, err)
		}
	}
	return nil
}
