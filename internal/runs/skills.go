package runs

import (
	"context"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/skills"
)

// SkillResolver returns the effective skills of one run, narrow first.
type SkillResolver interface {
	Resolve(ctx context.Context, rs skills.RunScope) ([]skills.Resolved, error)
}

// WithSkills wires the skill resolver. Nil keeps global skills only.
func (s *Service) WithSkills(r SkillResolver) *Service {
	s.skills = r
	return s
}

// resolveSkills sets the effective skill set of a launch: the skills of the
// job, its agent, its workspace, and the global skills.
func (s *Service) resolveSkills(ctx context.Context, opts *LaunchOpts) {
	if s.skills == nil || opts.WorkspaceID == "" {
		return
	}
	rs := skills.RunScope{WorkspaceID: opts.WorkspaceID}
	switch opts.Kind {
	case KindAgent:
		rs.AgentID = opts.JobID
	case KindJob, "":
		rs.JobID = opts.JobID
	}
	list, err := s.skills.Resolve(ctx, rs)
	if err != nil {
		s.log.Warn("resolve skills", "job_id", opts.JobID, "err", err)
		return
	}
	opts.Skills = list
}

// scopedSkills are the used skills narrower than global. Global skills
// reach the harness through the home projection.
func scopedSkills(list []skills.Resolved) []skills.Resolved {
	var out []skills.Resolved
	for _, r := range list {
		if r.Used && r.Scope != skills.ScopeGlobal {
			out = append(out, r)
		}
	}
	return out
}

// wireSkills are the scoped skills as agent-wire session skills.
func wireSkills(list []skills.Resolved) []agentwire.Skill {
	var out []agentwire.Skill
	for _, r := range scopedSkills(list) {
		out = append(out, agentwire.Skill{Name: r.Name, Dir: r.Path})
	}
	return out
}

// scopedSkillHint names the scoped skills in the prompt. A pane run has no
// other way to get them, and the note tells a session that loaded a global
// skill of the same name which copy wins.
func scopedSkillHint(list []skills.Resolved) string {
	scoped := scopedSkills(list)
	if len(scoped) == 0 {
		return ""
	}
	lines := []string{"Skills for this run. Read SKILL.md in the folder before you use one. When a skill of the same name is also installed elsewhere, use the copy here:"}
	for _, r := range scoped {
		line := "- " + r.Name + " (" + string(r.Scope) + " skill, " + r.Path + ")"
		if r.Description != "" {
			line += ": " + r.Description
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// usedSkillNames are the names of the effective skill set, or nil when the
// set is unknown.
func usedSkillNames(list []skills.Resolved) map[string]bool {
	if list == nil {
		return nil
	}
	out := map[string]bool{}
	for _, r := range list {
		if r.Used {
			out[strings.ToLower(r.Name)] = true
		}
	}
	return out
}

// recordSkills keeps the refs of the effective skill set on the run.
func (s *Service) recordSkills(ctx context.Context, runID string, list []skills.Resolved) {
	var refs []string
	for _, r := range list {
		if r.Used {
			refs = append(refs, r.Ref)
		}
	}
	if len(refs) == 0 {
		return
	}
	s.AddEvent(ctx, runID, "skills", "runner", map[string]any{"refs": refs})
}
