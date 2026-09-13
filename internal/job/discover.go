package job

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Discovery is the result of one workspace walk.
type Discovery struct {
	Jobs      []*Job
	Assistant *Job
	Agents    []*Job
}

// Discover walks a workspace root and returns parsed jobs (JOB.md), the
// optional root assistant (TREBI.md), and agents (AGENT.md).
// Broken files still appear with ParseError set.
// otherWorkspaceRoots are registered workspace roots to skip (nested workspaces, B10).
func Discover(workspacePath string, otherWorkspaceRoots ...string) (Discovery, error) {
	skipRoots := make(map[string]bool, len(otherWorkspaceRoots))
	for _, r := range otherWorkspaceRoots {
		if r != "" {
			skipRoots[filepath.Clean(r)] = true
		}
	}
	dv := &discovery{workspacePath: workspacePath, skipRoots: skipRoots, seenAgents: map[string]bool{}}
	var disc Discovery
	err := filepath.WalkDir(workspacePath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if dv.skipDir(path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		switch {
		case strings.EqualFold(base, "JOB.md"):
			if j := dv.job(path); j != nil {
				disc.Jobs = append(disc.Jobs, j)
			}
		case strings.EqualFold(base, "TREBI.md") && filepath.Dir(path) == workspacePath:
			if j := dv.assistant(path); j != nil {
				disc.Assistant = j
			}
		case strings.EqualFold(base, "AGENT.md"):
			if j := dv.agent(path); j != nil {
				disc.Agents = append(disc.Agents, j)
			}
		}
		return nil
	})
	return disc, err
}

// discovery holds the walk state for one workspace root.
type discovery struct {
	workspacePath string
	skipRoots     map[string]bool
	seenAgents    map[string]bool // lowercase index names (C.2 duplicate rule)
}

// skipDir reports whether the walk must not enter the directory.
func (dv *discovery) skipDir(path, name string) bool {
	if name == ".git" || name == "node_modules" || name == ".trebi" {
		return true
	}
	if path == dv.workspacePath {
		return false
	}
	// Registered nested workspace root (B10).
	if dv.skipRoots[filepath.Clean(path)] {
		return true
	}
	// Nested workspace: child trebi.yaml means separate workspace (B26).
	if _, err := os.Stat(filepath.Join(path, "trebi.yaml")); err == nil {
		return true
	}
	return false
}

// job parses a JOB.md and sets its workspace-relative name and owner.
func (dv *discovery) job(path string) *Job {
	// AGENT.md + JOB.md in one directory is the agent's error case (C.2):
	// the AGENT.md row carries the parse error, the JOB.md is not indexed.
	if siblingExists(filepath.Dir(path), "AGENT.md") {
		return nil
	}
	j, parseErr := ParseFile(path)
	if j == nil {
		return nil
	}
	// Relative name from workspace root.
	rel, _ := filepath.Rel(dv.workspacePath, filepath.Dir(path))
	if rel == "." || rel == "" {
		j.Name = filepath.Base(filepath.Dir(path))
		if dv.workspacePath == filepath.Dir(path) {
			// JOB.md at workspace root — use parent folder name
			j.Name = filepath.Base(dv.workspacePath)
		}
	} else {
		j.Name = filepath.ToSlash(rel)
		j.Owner = ownerForRelDir(rel)
	}
	j.WorkspacePath = dv.workspacePath
	if parseErr != nil && j.ParseError == "" {
		j.ParseError = parseErr.Error()
	}
	return j
}

// assistant parses the root TREBI.md.
func (dv *discovery) assistant(path string) *Job {
	j, parseErr := ParseFile(path)
	if j == nil {
		return nil
	}
	j.WorkspacePath = dv.workspacePath
	if parseErr != nil && j.ParseError == "" {
		j.ParseError = parseErr.Error()
	}
	return j
}

// agent parses an AGENT.md, names it agents/<name>, applies the C.2 layout
// rules, and expands {{agent_dir}}/{{workspace_dir}} (C.3).
func (dv *discovery) agent(path string) *Job {
	j, parseErr := ParseFile(path)
	if j == nil {
		return nil
	}
	dir := filepath.Dir(path)
	rel, _ := filepath.Rel(dv.workspacePath, dir)
	if rel == "." || rel == "" {
		j.Name = filepath.Base(dv.workspacePath)
	} else {
		j.Name = filepath.ToSlash(rel)
	}
	j.WorkspacePath = dv.workspacePath
	j.Kind = "agent"
	if parseErr != nil && j.ParseError == "" {
		j.ParseError = parseErr.Error()
	}
	dv.validateAgent(j, rel, dir)
	j.Prompt = SubstituteAgentVars(j.Prompt, dir, dv.workspacePath)
	if dv.seenAgents[strings.ToLower(j.Name)] {
		j.ParseError = JoinParseError(j.ParseError, fmt.Sprintf("duplicate agent name %s", j.Name))
	}
	dv.seenAgents[strings.ToLower(j.Name)] = true
	return j
}

var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validateAgent applies the C.2 layout rules, appending to ParseError.
func (dv *discovery) validateAgent(j *Job, rel, dir string) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	layout := len(parts) == 2 && parts[0] == "agents" && parts[1] != ""
	switch {
	case len(parts) >= 3 && parts[0] == "agents":
		j.ParseError = JoinParseError(j.ParseError, "nested agent root: put AGENT.md directly in agents/<name>")
	case !layout:
		j.ParseError = JoinParseError(j.ParseError, "misplaced AGENT.md: expected <workspace>/agents/<name>/AGENT.md")
	}
	if layout && !agentNameRe.MatchString(parts[1]) {
		j.ParseError = JoinParseError(j.ParseError, fmt.Sprintf("agent folder name %q must match [a-z0-9][a-z0-9-]*", parts[1]))
	}
	if siblingExists(dir, "JOB.md") {
		j.ParseError = JoinParseError(j.ParseError, "AGENT.md and JOB.md in the same directory")
	}
	if siblingExists(dir, "SOUL.md") {
		j.ParseError = JoinParseError(j.ParseError, "SOUL.md beside AGENT.md is not accepted")
	}
}

// SubstituteAgentVars expands the AGENT.md body placeholders (C.3).
// {{agent_jobs}} is wake-time (Part F) and stays literal here.
func SubstituteAgentVars(body, agentDir, workspaceDir string) string {
	return strings.NewReplacer("{{agent_dir}}", agentDir, "{{workspace_dir}}", workspaceDir).Replace(body)
}

// ownerForRelDir returns the owning agent index name for a workspace-relative
// job directory: agents/<seg>/… → agents/<seg>, else "" (Part D). Pure path
// rule; doctor reports jobs whose owner row is gone.
func ownerForRelDir(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 && parts[0] == "agents" && parts[1] != "" {
		return "agents/" + parts[1]
	}
	return ""
}

func siblingExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// Doctor layout codes (Part K). The doctor reports these from index rows.
const (
	CodeAgentMisplaced   = "agent_misplaced"
	CodeAgentNested      = "agent_nested"
	CodeAgentRootJob     = "agent_root_job"
	CodeAgentOrphanedJob = "agent_orphaned_jobs"
)

// LayoutRow is one job_index row flattened for layout checks.
type LayoutRow struct {
	WorkspaceID, Name, Path, Kind, Owner string
}

// LayoutIssue is one agent layout problem found in index rows.
type LayoutIssue struct {
	Code, Path string
}

// Severity returns the doctor severity of the issue.
func (i LayoutIssue) Severity() string {
	switch i.Code {
	case CodeAgentNested, CodeAgentRootJob:
		return "error"
	default:
		return "warn"
	}
}

// Message names the path and the rule the path breaks.
func (i LayoutIssue) Message() string {
	switch i.Code {
	case CodeAgentNested:
		return fmt.Sprintf("AGENT.md inside another agent: %s. Put AGENT.md directly in agents/<name>/.", i.Path)
	case CodeAgentRootJob:
		return fmt.Sprintf("JOB.md next to AGENT.md is not indexed: %s. Move JOB.md into a subfolder of the agent.", i.Path)
	case CodeAgentOrphanedJob:
		return fmt.Sprintf("job with no AGENT.md owner: %s. Move the job out, or recreate the agent.", i.Path)
	default:
		return fmt.Sprintf("AGENT.md outside agents/<name>/: %s. Put AGENT.md directly in agents/<name>/.", i.Path)
	}
}

// LayoutIssues classifies agent layout problems in index rows. It mirrors
// validateAgent from the index side: the doctor reads rows, not the walk.
// It stats the filesystem once per agent row (the root JOB.md sibling).
func LayoutIssues(rows []LayoutRow) []LayoutIssue {
	okAgents := map[string]bool{}
	for _, r := range rows {
		if r.Kind == "agent" && agentIndexNameShape(r.Name) {
			okAgents[r.WorkspaceID+"\x00"+r.Name] = true
		}
	}
	var out []LayoutIssue
	for _, r := range rows {
		if r.Kind == "agent" {
			out = append(out, r.agentIssues()...)
			continue
		}
		if r.Kind != "job" {
			continue
		}
		// The stamped owner, or the agents/<name> root the name sits at.
		parts := strings.Split(r.Name, "/")
		owner := r.Owner
		if owner == "" && len(parts) == 2 && parts[0] == "agents" && parts[1] != "" {
			owner = r.Name
		}
		if owner != "" && !okAgents[r.WorkspaceID+"\x00"+owner] {
			out = append(out, LayoutIssue{Code: CodeAgentOrphanedJob, Path: r.Path})
		}
	}
	return out
}

// agentIssues classifies one agent row: nested root, misplaced file, or a
// JOB.md sibling that discovery refused to index.
func (r LayoutRow) agentIssues() []LayoutIssue {
	var out []LayoutIssue
	parts := strings.Split(r.Name, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "agents":
		out = append(out, LayoutIssue{Code: CodeAgentNested, Path: r.Path})
	case !agentIndexNameShape(r.Name):
		out = append(out, LayoutIssue{Code: CodeAgentMisplaced, Path: r.Path})
	}
	if siblingExists(filepath.Dir(r.Path), "JOB.md") {
		out = append(out, LayoutIssue{Code: CodeAgentRootJob, Path: r.Path})
	}
	return out
}

// JoinParseError merges two parse errors, dropping empties and repeats. The
// API layer uses it to keep an index row's layout error beside a live parse
// result; the same file error seen by both must not print twice.
func JoinParseError(existing, add string) string {
	switch {
	case existing == "":
		return add
	case add == "" || add == existing:
		return existing
	default:
		return existing + "; " + add
	}
}

// agentIndexNameShape reports whether a name is a canonical agents/<name>.
func agentIndexNameShape(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 2 && parts[0] == "agents" && parts[1] != ""
}
