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
	row, err := movableJob(ws, existing, name)
	if err != nil {
		return JobRenamed{}, nil, err
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

// ErrNoAgent is returned when a move names an agent that does not exist.
var ErrNoAgent = errors.New("target agent not found")

// JobMove is the target of a job move. Agent "" is a job with no agent.
// Folder "" is no folder; a job with no agent cannot have one. A nil
// Agent keeps the owner. A nil Folder keeps the folder when the owner
// stays, else it is no folder.
type JobMove struct {
	Agent  *string `json:"agent,omitempty"`
	Folder *string `json:"folder,omitempty"`
	DryRun bool    `json:"dry_run,omitempty"`
}

// JobMoved is the reply of a job move. Changes holds plain notes about
// what the job gets from its new agent; a dry run fills it too.
type JobMoved struct {
	ID        string   `json:"id"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	FromAgent string   `json:"from_agent,omitempty"`
	ToAgent   string   `json:"to_agent,omitempty"`
	Folder    string   `json:"folder,omitempty"`
	Moved     bool     `json:"moved"`
	Changes   []string `json:"changes,omitempty"`
}

// MoveJob gives a job another owner agent, another folder, or both. An
// owner change moves the job folder on disk; the job keeps its ID, so
// its schedules, state, memory, and runs stay. It returns the parsed
// jobs that moved, for the schedules.
func (s *Service) MoveJob(ctx context.Context, workspaceID, name string, m JobMove) (JobMoved, []*job.Job, error) {
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return JobMoved{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.existingIndexRows(ctx, ws.ID)
	if err != nil {
		return JobMoved{}, nil, err
	}
	t, err := resolveMove(ws, existing, name, m)
	if err != nil {
		return JobMoved{}, nil, err
	}
	out := t.reply()
	if m.DryRun || (t.toRel == t.row.name && t.folder == t.row.folder) {
		return out, nil, nil
	}
	moved, err := s.applyMove(ctx, ws, existing, t)
	if err != nil {
		return JobMoved{}, nil, err
	}
	out.Moved = true
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "job.moved", Entity: "job", EntityID: t.row.id, Action: "moved",
		Attrs: map[string]string{
			"workspace": ws.ID, "job_id": t.row.id, "job_name": t.toRel, "from": t.row.name, "to": t.toRel,
			"from_agent": out.FromAgent, "to_agent": out.ToAgent, "folder": t.folder,
		},
	}); err != nil {
		s.log.Warn("job move bus append failed", "workspace", ws.ID, "error", err)
	}
	return out, moved, nil
}

// movableJob returns the index row of a job that a rename or a move can
// relocate: a job (not an agent) below the workspace root.
func movableJob(ws *Workspace, existing map[string]indexRow, name string) (indexRow, error) {
	var row indexRow
	for _, r := range existing {
		if r.name == name || r.id == name {
			row = r
			break
		}
	}
	switch {
	case row.id == "":
		return row, ErrNotFound
	case row.kind == "agent":
		return row, ErrNotJob
	case row.kind != "job":
		return row, ErrNotFound
	case filepath.Clean(filepath.Dir(row.path)) == filepath.Clean(ws.Path):
		return row, ErrRootJob
	}
	return row, nil
}

// moveTarget is a checked job move: the job row, both owner rows (empty
// for no agent), and the new name and folder.
type moveTarget struct {
	row, from, to indexRow
	toRel, folder string
}

// resolveMove finds the job and its new owner and checks the move.
func resolveMove(ws *Workspace, existing map[string]indexRow, name string, m JobMove) (moveTarget, error) {
	var t moveTarget
	var err error
	if t.row, err = movableJob(ws, existing, name); err != nil {
		return t, err
	}
	t.from = existing[t.row.ownerID]
	agent := strings.TrimPrefix(t.from.name, "agents/")
	if m.Agent != nil {
		agent = strings.TrimPrefix(strings.TrimSpace(*m.Agent), "agents/")
	}
	for _, r := range existing {
		if r.kind == "agent" && r.name == "agents/"+agent {
			t.to = r
		}
	}
	if agent != "" && t.to.id == "" {
		return t, ErrNoAgent
	}
	same := t.to.id == t.from.id
	if m.Folder != nil {
		if t.folder, err = job.NormalizeFolder(*m.Folder); err != nil {
			return t, err
		}
	} else if same {
		t.folder = t.row.folder
	}
	if agent == "" && t.folder != "" {
		return t, job.ErrFolderNoOwner
	}
	switch {
	case same:
		t.toRel = t.row.name
	case agent == "":
		t.toRel = path.Base(t.row.name)
	default:
		t.toRel = path.Join(t.to.name, path.Base(t.row.name))
	}
	return t, nil
}

func (t moveTarget) reply() JobMoved {
	out := JobMoved{
		ID: t.row.id, From: t.row.name, To: t.toRel, Folder: t.folder,
		FromAgent: strings.TrimPrefix(t.from.name, "agents/"), ToAgent: strings.TrimPrefix(t.to.name, "agents/"),
	}
	if t.from.id != t.to.id {
		out.Changes = ownerChanges(t.row, t.from, t.to)
	}
	return out
}

// applyMove writes the folder key and, on an owner change, moves the job
// folder with the index. The caller holds s.mu.
func (s *Service) applyMove(ctx context.Context, ws *Workspace, existing map[string]indexRow, t moveTarget) ([]*job.Job, error) {
	setFolder := func(dir string) error {
		return job.MergePatchFrontmatter(filepath.Join(dir, filepath.Base(t.row.path)), map[string]any{"folder": t.folder})
	}
	if t.toRel != t.row.name {
		return s.relocateObject(ctx, ws, existing, relocation{fromRel: t.row.name, toRel: t.toRel, afterMove: setFolder})
	}
	if err := setFolder(filepath.Dir(t.row.path)); err != nil {
		return nil, err
	}
	delete(s.fp, ws.ID)
	return nil, nil
}

// ownerChanges lists, in plain words, what a job gets from a new owner
// agent. A file that does not parse adds no note.
func ownerChanges(row, from, to indexRow) []string {
	short := func(r indexRow) string { return strings.TrimPrefix(r.name, "agents/") }
	notes := []string{"The job runs as " + short(to) + ". It uses the settings and the email address of " + short(to) + "."}
	if to.id == "" {
		notes[0] = "The job no longer belongs to an agent. It does not use the settings or the email address of " + short(from) + "."
	}
	if j, _ := job.ParseFile(row.path); j != nil && len(j.Connectors) == 0 {
		lost, gained := kitDiff(kitConnectors(from), kitConnectors(to))
		if len(lost) > 0 {
			notes = append(notes, "The job loses these connectors: "+strings.Join(lost, ", ")+".")
		}
		if len(gained) > 0 {
			notes = append(notes, "The job gets these connectors: "+strings.Join(gained, ", ")+".")
		}
	}
	if from.id != "" {
		if data, err := os.ReadFile(from.path); err == nil && strings.Contains(string(data), path.Base(row.name)) {
			notes = append(notes, "The instructions of "+short(from)+" name this job. Update them.")
		}
	}
	return notes
}

// kitConnectors returns the kit connectors of an agent row, or nil.
func kitConnectors(agent indexRow) []string {
	if agent.id == "" {
		return nil
	}
	a, _ := job.ParseFile(agent.path)
	if a == nil || a.Kit == nil {
		return nil
	}
	return a.Kit.Connectors
}

// kitDiff returns the entries of have that want lacks, and the reverse.
func kitDiff(have, want []string) (lost, gained []string) {
	for _, c := range have {
		if !slices.Contains(want, c) {
			lost = append(lost, c)
		}
	}
	for _, c := range want {
		if !slices.Contains(have, c) {
			gained = append(gained, c)
		}
	}
	return lost, gained
}

// FolderRenamed is the reply of a folder rename.
type FolderRenamed struct {
	Agent string `json:"agent"`
	From  string `json:"from"`
	To    string `json:"to"`
	Jobs  int    `json:"jobs"`
}

// RenameFolder changes the folder of each job of an agent in folder from.
// To "" removes the folder from those jobs.
func (s *Service) RenameFolder(ctx context.Context, workspaceID, agent, from, to string) (FolderRenamed, error) {
	agent = strings.TrimPrefix(strings.TrimSpace(agent), "agents/")
	from = strings.TrimSpace(from)
	to, err := job.NormalizeFolder(to)
	if err != nil {
		return FolderRenamed{}, err
	}
	if from == "" {
		return FolderRenamed{}, fmt.Errorf("from is required")
	}
	ws, err := s.Get(ctx, workspaceID)
	if err != nil {
		return FolderRenamed{}, err
	}
	s.mu.Lock()
	existing, err := s.existingIndexRows(ctx, ws.ID)
	if err != nil {
		s.mu.Unlock()
		return FolderRenamed{}, err
	}
	agentID := ""
	for id, r := range existing {
		if r.kind == "agent" && r.name == "agents/"+agent {
			agentID = id
		}
	}
	if agentID == "" {
		s.mu.Unlock()
		return FolderRenamed{}, ErrNoAgent
	}
	out := FolderRenamed{Agent: agent, From: from, To: to}
	for _, r := range existing {
		if r.kind != "job" || r.ownerID != agentID || r.folder != from {
			continue
		}
		if err := job.MergePatchFrontmatter(r.path, map[string]any{"folder": to}); err != nil {
			s.mu.Unlock()
			return out, fmt.Errorf("set folder of %s: %w", r.name, err)
		}
		out.Jobs++
	}
	s.mu.Unlock()
	if out.Jobs == 0 {
		return out, ErrNotFound
	}
	return out, s.RebuildIndex(ctx, ws)
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
