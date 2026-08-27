package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
)

// handleRunsDryRun evaluates when: gates on schedule triggers without launching (B25).
func (s *Server) handleRunsDryRun(w http.ResponseWriter, r *http.Request,
	workspace, jobName, kind string, reason runs.RunReason, harness, prompt string, inputs map[string]string,
) {
	ctx := r.Context()
	// Resolve job file through the A4 resolver.
	var j *job.Job
	var path string
	if row, err := s.findJobRow(ctx, workspace, jobName); err == nil {
		path = row.Path
		if parsed, perr := job.ParseFile(row.Path); perr == nil {
			j = parsed
		}
		if workspace == "" {
			workspace = row.WorkspaceID
		}
	}
	verdicts := []map[string]any{}
	fire := true
	var vars map[string]string
	if j != nil {
		if harness == "" {
			harness = j.Harness
		}
		if prompt == "" {
			prompt = j.Prompt
		}
		verdicts, fire, vars = scheduleVerdicts(ctx, j)
	}
	// Apply extracted vars to prompt for preview
	preview := prompt
	for k, v := range vars {
		preview = strings.ReplaceAll(preview, "{{"+k+"}}", v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run":        true,
		"fire":           fire,
		"workspace":      workspace,
		"job":            jobName,
		"kind":           kind,
		"reason":         reason,
		"harness":        harness,
		"prompt_preview": truncStr(preview, 500),
		"inputs":         inputs,
		"path":           path,
		"when":           verdicts,
		"vars":           vars,
	})
}

// scheduleVerdicts evaluates the when: gate of each cron/every trigger.
func scheduleVerdicts(ctx context.Context, j *job.Job) (verdicts []map[string]any, fire bool, vars map[string]string) {
	verdicts = []map[string]any{}
	fire = true
	cwd := j.WorkingDir
	if cwd == "" {
		cwd = j.Dir
	}
	for _, tr := range j.Triggers {
		if tr.Family != "cron" && tr.Family != "every" {
			continue
		}
		if tr.When == nil {
			verdicts = append(verdicts, map[string]any{
				"family": tr.Family, "index": tr.Index, "fire": true, "skip": false,
				"note": "no when: gate",
			})
			continue
		}
		wr := job.EvalWhen(ctx, tr.When, cwd, j.Env)
		if wr.Vars != nil {
			vars = wr.Vars
		}
		verdicts = append(verdicts, whenVerdict(tr, wr))
		if wr.Skip || !wr.Fire || wr.Error != "" {
			fire = false
		}
	}
	return verdicts, fire, vars
}

// whenVerdict maps one when: evaluation to its wire object.
func whenVerdict(tr job.Trigger, wr job.WhenResult) map[string]any {
	v := map[string]any{
		"family": tr.Family, "index": tr.Index,
		"fire": wr.Fire, "skip": wr.Skip || !wr.Fire,
	}
	if wr.Error != "" {
		v["error"] = wr.Error
	}
	if wr.Vars != nil {
		v["vars"] = wr.Vars
	}
	if wr.Stdout != "" {
		v["stdout"] = truncStr(wr.Stdout, 500)
	}
	return v
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
