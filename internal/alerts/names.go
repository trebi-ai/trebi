package alerts

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
)

// Names are the display names of one alert. No field holds an ID.
type Names struct {
	Agent      string // the agent title, for an agent or for a job of an agent
	Job        string // the job title; "" for an agent
	Workspace  string
	Connection string
	App        string // the AI app label
}

// Owner is the name that starts a subject: the job, else the agent.
func (n Names) Owner() string { return cmp.Or(n.Job, n.Agent) }

// Details is the muted line below the mail body.
func (n Names) Details() string {
	var parts []string
	for _, p := range []string{n.Agent, n.Job, n.Connection, n.Workspace, n.App} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}

// named fills the job refs and the display names of o once.
func (s *Service) named(ctx context.Context, o *RaiseOpts) error {
	if o.named {
		return nil
	}
	if err := s.fillJob(ctx, o); err != nil {
		return err
	}
	o.names = Names{
		Workspace:  s.workspaceName(ctx, o.WorkspaceID),
		Connection: s.connectionTitle(o.Connector),
		App:        appLabel(o.Harness),
	}
	o.names.Agent, o.names.Job = s.jobTitles(ctx, o.WorkspaceID, o.JobName)
	o.named = true
	return nil
}

func (s *Service) workspaceName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	var name string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id=?`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}

// jobTitles gives the agent title and the job title of an index name. An
// agent (agents/<a>) has no job title. A job of an agent has both.
func (s *Service) jobTitles(ctx context.Context, workspaceID, name string) (agent, jobTitle string) {
	if name == "" {
		return "", ""
	}
	short, isAgentPath := strings.CutPrefix(name, "agents/")
	if isAgentPath && !strings.Contains(short, "/") {
		return s.fileTitle(ctx, workspaceID, name), ""
	}
	if isAgentPath {
		agentName, _, _ := strings.Cut(short, "/")
		agent = s.fileTitle(ctx, workspaceID, "agents/"+agentName)
	}
	return agent, s.fileTitle(ctx, workspaceID, name)
}

// fileTitle is the title of the JOB.md or AGENT.md of an index name, else
// its last path segment.
func (s *Service) fileTitle(ctx context.Context, workspaceID, name string) string {
	var file string
	err := s.st.DB().QueryRowContext(ctx, `SELECT path FROM job_index WHERE workspace_id=? AND name=?`,
		workspaceID, name).Scan(&file)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Debug("job title", "job", name, "error", err)
	}
	return titleOf(file, name)
}

// titleOf is the title in a JOB.md or AGENT.md file, else the last path
// segment of name.
func titleOf(file, name string) string {
	if file == "" {
		return path.Base(name)
	}
	j, err := job.ParseFile(file)
	if err != nil || strings.TrimSpace(j.Title) == "" {
		return path.Base(name)
	}
	return strings.TrimSpace(j.Title)
}

// connectionTitle is the title of a connection, else "". A bare connection
// ID never shows.
func (s *Service) connectionTitle(ref string) string {
	if ref == "" {
		return ""
	}
	if s.opts.ConnectionTitle != nil {
		if t := s.opts.ConnectionTitle(ref); t != "" {
			return t
		}
	}
	if strings.HasPrefix(ref, "con_") {
		return ""
	}
	return ref
}

// appLabel is the label of an AI app, else its name.
func appLabel(name string) string {
	return cmp.Or(harness.Label(name), name)
}

// OwnerTitle is the display title of a job or an agent by its index
// name: the job title, else the agent title, else the short name.
func (s *Service) OwnerTitle(ctx context.Context, workspaceID, name string) string {
	agent, jobTitle := s.jobTitles(ctx, workspaceID, name)
	return cmp.Or(jobTitle, agent, path.Base(name))
}

// agentTitle is the title of an agent by its short name.
func (s *Service) agentTitle(ctx context.Context, workspaceID, short string) string {
	if short == "" {
		return ""
	}
	return s.fileTitle(ctx, workspaceID, "agents/"+short)
}

// agentByTag is the title of the agent with the mail tag, and whether one
// exists. An empty workspace matches any workspace.
func (s *Service) agentByTag(ctx context.Context, workspaceID, tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	var ws string
	err := s.st.DB().QueryRowContext(ctx, `SELECT workspace_id FROM job_index WHERE name=? AND (?='' OR workspace_id=?)
		AND retired_at IS NULL LIMIT 1`, "agents/"+tag, workspaceID, workspaceID).Scan(&ws)
	if err != nil {
		return "", false
	}
	return s.agentTitle(ctx, ws, tag), true
}

// PausedBody is the body of a harness_paused item. A zero until means no
// end.
func (s *Service) PausedBody(harness string, until time.Time) string {
	app := appLabel(harness)
	if until.IsZero() {
		return "Jobs that use " + app + " wait until you resume them."
	}
	return "Jobs that use " + app + " wait until the limit resets " + s.at(until) + "."
}

// When is a short local time: "9:00 AM" today, else "Nov 5, 9:00 AM".
func (s *Service) when(t time.Time) string {
	t, now := t.Local(), s.opts.Now().Local()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("3:04 PM")
	case t.Year() == now.Year():
		return t.Format("Jan 2, 3:04 PM")
	}
	return t.Format("Jan 2 2006, 3:04 PM")
}

// at is "at 9:00 AM" today, else "on Nov 5, 9:00 AM".
func (s *Service) at(t time.Time) string {
	w := s.when(t)
	if strings.Contains(w, ",") {
		return "on " + w
	}
	return "at " + w
}

// runStart is the start time of a run, or zero.
func (s *Service) runStart(ctx context.Context, runID string) time.Time {
	if runID == "" {
		return time.Time{}
	}
	var started string
	if s.st.DB().QueryRowContext(ctx, `SELECT started_at FROM runs WHERE id=?`, runID).Scan(&started) != nil {
		return time.Time{}
	}
	return parseTime(started)
}

// runOf names a run by its owner and start: "the run of Car Hunt at
// 9:00 AM".
func (s *Service) runOf(ctx context.Context, who, runID string) string {
	text := "the run"
	if who != "" {
		text += " of " + who
	}
	if t := s.runStart(ctx, runID); !t.IsZero() {
		text += " at " + s.when(t)
	}
	return text
}

// capFirst makes the first letter upper case.
func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// toolVerbs map the tool of a permission to plain words.
var toolVerbs = map[string]string{ //nolint:gochecknoglobals // static table
	"Bash":         "run a command",
	"Edit":         "change files",
	"MultiEdit":    "change files",
	"Write":        "change files",
	"NotebookEdit": "change files",
	"Read":         "read files",
	"Glob":         "search files",
	"Grep":         "search files",
	"WebFetch":     "open a web page",
	"WebSearch":    "search the web",
	"Task":         "start a helper agent",
	"Agent":        "start a helper agent",
}

// toolVerb is what a tool does, in plain words.
func (s *Service) toolVerb(tool string) string {
	if v, ok := toolVerbs[tool]; ok {
		return v
	}
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		server, name, _ := strings.Cut(rest, "__")
		title := cmp.Or(s.connectionTitle(server), server)
		if name == "" {
			return "use " + title
		}
		return "use " + title + " to " + strings.ReplaceAll(name, "_", " ")
	}
	return "use the tool " + tool
}

// approvalText is the title and the body of a permission question.
func (s *Service) approvalText(who, question string) (title, body string) {
	who = cmp.Or(who, "An agent")
	tool, ok := strings.CutPrefix(question, "Allow ")
	tool, ok2 := strings.CutSuffix(tool, "?")
	switch {
	case ok && ok2 && tool == "AskUserQuestion":
		return who + " asks you a question", who + " has a question and waits for your answer."
	case ok && ok2 && !strings.Contains(tool, " "):
		v := s.toolVerb(tool)
		return who + " needs your OK to " + v, who + " wants to " + v + " on this machine and waits for your OK."
	case question == "Tool permission requested":
		return who + " needs your OK to use a tool", who + " wants to use a tool and waits for your OK."
	}
	return who + " needs your OK", who + " waits for your OK: " + question
}

// approvalHint tells how to answer a permission question.
func approvalHint(title string) string {
	if strings.HasSuffix(title, " asks you a question") {
		return "Answer in Trebi."
	}
	return "Reply approve or deny to this email. Or answer in Trebi."
}

// endReasons map the end reason of a run to plain words.
var endReasons = map[string]string{ //nolint:gochecknoglobals // static table
	"failed":            "The agent stopped with an error.",
	"session_gone":      "The agent session went away before the run ended.",
	"idle_timeout":      "The agent did nothing for too long, so Trebi stopped it.",
	"max_duration":      "The run took longer than its time limit, so Trebi stopped it.",
	"launch":            "The agent did not start.",
	"auth":              "The AI app needs a new login.",
	"limit":             "The AI app reached its usage limit.",
	"kill":              "Someone stopped the run.",
	"harness_off":       "The AI app of the job is off.",
	"connector_login":   "A connection of the job needs a new login.",
	"request_dismissed": "Someone dismissed its question.",
	"restart":           "Trebi restarted during the run.",
}

// endReasonText is the end reason in plain words.
func endReasonText(reason string) string {
	if t, ok := endReasons[reason]; ok {
		return t
	}
	if reason == "" {
		return ""
	}
	return "It ended with the reason " + strings.ReplaceAll(reason, "_", " ") + "."
}
