package job

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Discover walks a workspace root and returns parsed jobs (JOB.md) and optional assistant (TREBI.md).
// Broken files still appear with ParseError set.
// otherWorkspaceRoots are registered workspace roots to skip (nested workspaces, B10).
func Discover(workspacePath string, otherWorkspaceRoots ...string) (jobs []*Job, assistant *Job, err error) {
	skipRoots := make(map[string]bool, len(otherWorkspaceRoots))
	for _, r := range otherWorkspaceRoots {
		if r != "" {
			skipRoots[filepath.Clean(r)] = true
		}
	}
	dv := discovery{workspacePath: workspacePath, skipRoots: skipRoots}
	err = filepath.WalkDir(workspacePath, func(path string, d fs.DirEntry, walkErr error) error {
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
				jobs = append(jobs, j)
			}
		case strings.EqualFold(base, "TREBI.md") && filepath.Dir(path) == workspacePath:
			if j := dv.assistant(path); j != nil {
				assistant = j
			}
		}
		return nil
	})
	return jobs, assistant, err
}

// discovery holds the walk state for one workspace root.
type discovery struct {
	workspacePath string
	skipRoots     map[string]bool
}

// skipDir reports whether the walk must not enter the directory.
func (dv discovery) skipDir(path, name string) bool {
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

// job parses a JOB.md and sets its workspace-relative name.
func (dv discovery) job(path string) *Job {
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
	}
	j.WorkspacePath = dv.workspacePath
	if parseErr != nil && j.ParseError == "" {
		j.ParseError = parseErr.Error()
	}
	return j
}

// assistant parses the root TREBI.md.
func (dv discovery) assistant(path string) *Job {
	j, parseErr := ParseFile(path)
	if j == nil {
		return nil
	}
	j.WorkspacePath = dv.workspacePath
	j.Kind = "assistant"
	if parseErr != nil && j.ParseError == "" {
		j.ParseError = parseErr.Error()
	}
	return j
}
