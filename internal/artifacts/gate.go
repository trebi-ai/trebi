package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tools"
)

// Marker sets the outcome note of a run.
type Marker interface {
	Mark(ctx context.Context, opts runs.MarkOpts) (*runs.Run, error)
}

// Alerter raises an inbox item.
type Alerter interface {
	Raise(ctx context.Context, opts alerts.RaiseOpts) (string, error)
}

// RunGate saves the JOB.md artifacts: entries of a run when it completes. It
// is a park gate that never parks a run.
type RunGate struct {
	svc    *Service
	mark   Marker
	alerts Alerter
	log    *slog.Logger
}

var _ runs.ParkGate = (*RunGate)(nil)

func NewRunGate(svc *Service, m Marker, a Alerter) *RunGate {
	return &RunGate{svc: svc, mark: m, alerts: a, log: slog.Default().With("source", "artifacts_gate")}
}

// WithAlerts wires the alerts service, which the daemon builds after the
// parker.
func (g *RunGate) WithAlerts(a Alerter) *RunGate {
	g.alerts = a
	return g
}

func (g *RunGate) Name() string { return "artifacts" }

func (g *RunGate) Open(context.Context, string) (waiting, ready bool, err error) {
	return false, false, nil
}

func (g *RunGate) Claim(context.Context, string) ([]runs.ParkDelivery, error) { return nil, nil }

func (g *RunGate) Settle(context.Context, string, []runs.ParkDelivery, runs.ResumeResult) error {
	return nil
}

func (g *RunGate) Stale(context.Context) ([]string, error) { return nil, nil }

func (g *RunGate) PromptHint(runs.LaunchOpts) string { return "" }

// RunEnded saves each entry that the run did not save itself. A missing
// file sets the outcome note and raises an inbox item; the run status
// stays.
func (g *RunGate) RunEnded(ctx context.Context, runID string, st runs.Status) {
	if st != runs.StatusComplete || g.svc.runs == nil || g.svc.jobs == nil {
		return
	}
	run, err := g.svc.runs.Get(ctx, runID)
	if err != nil || run.JobID == "" {
		return
	}
	entries := g.entries(ctx, run.JobID)
	if len(entries) == 0 {
		return
	}
	saved, err := g.savedNames(ctx, runID)
	if err != nil {
		g.log.Warn("artifact gate", "run_id", runID, "err", err)
		return
	}
	origin, err := g.svc.OriginFor(ctx, &tools.Actor{Kind: run.Kind, RunID: run.ID, JobID: run.JobID, Job: run.JobName}, "", "")
	if err != nil {
		return
	}
	var notes []string
	for _, e := range entries {
		if saved[e.Name] {
			continue
		}
		o := PutOpts{Workspace: run.Workspace, Name: e.Name, Title: e.Title, Origin: origin}
		o.Base = g.runBase(ctx, run.Workspace, origin, e.Name)
		if _, err := g.svc.PutFromFile(ctx, o, run.WorkingDir, e.Path); err != nil {
			notes = append(notes, gateNote(e, err))
		}
	}
	if len(notes) > 0 {
		g.report(ctx, run, strings.Join(notes, " "))
	}
}

// entries reads the artifacts: key of the JOB.md of a job.
func (g *RunGate) entries(ctx context.Context, jobID string) []job.ArtifactYAML {
	row, err := g.svc.jobs.JobByID(ctx, jobID)
	if err != nil {
		return nil
	}
	j, err := job.ParseFile(row.Path)
	if err != nil || j == nil {
		return nil
	}
	return j.Artifacts
}

// report sets the outcome note of the run and raises an inbox item.
func (g *RunGate) report(ctx context.Context, run *runs.Run, note string) {
	if g.mark != nil {
		if _, err := g.mark.Mark(ctx, runs.MarkOpts{ID: run.ID, Status: runs.StatusComplete, Note: note}); err != nil {
			g.log.Warn("artifact note", "run_id", run.ID, "err", err)
		}
	}
	if g.alerts != nil {
		if _, err := g.alerts.Raise(ctx, alerts.RaiseOpts{
			Kind: alerts.KindArtifactMissing, Title: "Artifact not saved", Body: note, RunID: run.ID,
			WorkspaceID: run.Workspace, JobID: run.JobID,
		}); err != nil {
			g.log.Warn("artifact alert", "run_id", run.ID, "err", err)
		}
	}
}

// runBase lets a run add to an artifact whose newest version came from a
// run of the same job: the weekly report case. A newer version from a
// person or a conversation stays stale (D4).
func (g *RunGate) runBase(ctx context.Context, ws string, origin Origin, name string) *int {
	var n int
	var kind string
	err := g.svc.st.DB().QueryRowContext(ctx, `SELECT a.latest_version, COALESCE(v.origin_kind, '') FROM artifacts a
  LEFT JOIN artifact_versions v ON v.artifact_id = a.id AND v.n = a.latest_version
  WHERE a.workspace_id=? AND a.owner_kind=? AND a.owner_id=? AND a.name=?`,
		ws, origin.OwnerKind, origin.OwnerID, name).Scan(&n, &kind)
	if err != nil || kind != OriginRun {
		return nil
	}
	return &n
}

func (g *RunGate) savedNames(ctx context.Context, runID string) (map[string]bool, error) {
	refs, err := g.svc.ByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range refs {
		out[r.Name] = true
	}
	return out, nil
}

func gateNote(e job.ArtifactYAML, err error) string {
	var ie *InvalidError
	var se *StaleError
	switch {
	case errors.As(err, &ie) && len(ie.Fields) == 1 && ie.Fields[0].Field == "from_path":
		return fmt.Sprintf("Artifact %s was not written.", e.Path)
	case errors.As(err, &se):
		return fmt.Sprintf("Artifact %s was not saved: version %d of %s is newer.", e.Path, se.Latest, e.Name)
	case errors.Is(err, ErrTooLarge):
		return fmt.Sprintf("Artifact %s was not saved: the file is too large.", e.Path)
	}
	return fmt.Sprintf("Artifact %s was not saved: %v.", e.Path, err)
}

// Continuer sends a request about an artifact to the agent that wrote it.
type Continuer interface {
	SendToConversation(ctx context.Context, convID, text string) error
	ResumeRun(ctx context.Context, runID, prompt string) (newRunID string, err error)
	AskAssistant(ctx context.Context, workspaceID, text string) (convID string, err error)
}

// WithContinuer wires Ask.
func (s *Service) WithContinuer(c Continuer) *Service {
	s.cont = c
	return s
}

// AskOpts is one request of the user about a version.
type AskOpts struct {
	ID        string
	Version   int
	Text      string
	Selection string
}

// AskResult names where the request went.
type AskResult struct {
	Via            string `json:"via"` // conversation | run | assistant
	ConversationID string `json:"conversation_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
}

const maxSelection = 2000

// Ask continues the origin of the version with the request of the user
// (B9). A user version goes to the newest agent version, else to the
// workspace assistant.
func (s *Service) Ask(ctx context.Context, o AskOpts) (AskResult, error) {
	text := strings.TrimSpace(o.Text)
	if text == "" {
		return AskResult{}, invalid("text", "is required")
	}
	if s.cont == nil {
		return AskResult{}, errors.New("ask is not wired")
	}
	a, err := s.Get(ctx, o.ID)
	if err != nil {
		return AskResult{}, err
	}
	n := o.Version
	if n == 0 {
		n = a.LatestVersion
	}
	v, err := s.Version(ctx, a.ID, n)
	if err != nil {
		return AskResult{}, err
	}
	prompt := askPrompt(a.Name, n, o.Selection, text)
	if v.OriginKind == OriginUser {
		v, err = s.lastAgentVersion(ctx, a.ID)
		if err != nil {
			return AskResult{}, err
		}
	}
	switch {
	case v.ConversationID != "":
		return AskResult{Via: OriginConversation, ConversationID: v.ConversationID},
			s.cont.SendToConversation(ctx, v.ConversationID, prompt)
	case v.RunID != "":
		id, err := s.cont.ResumeRun(ctx, v.RunID, prompt)
		return AskResult{Via: OriginRun, RunID: id}, err
	}
	conv, err := s.cont.AskAssistant(ctx, a.Workspace, prompt)
	return AskResult{Via: OwnerAssistant, ConversationID: conv}, err
}

// lastAgentVersion is the newest version that a run or a conversation
// saved. It is empty when the user saved every version.
func (s *Service) lastAgentVersion(ctx context.Context, id string) (Version, error) {
	v, err := scanVersion(s.st.DB().QueryRowContext(ctx, versionSelect+` WHERE v.artifact_id=? AND v.origin_kind<>? ORDER BY v.n DESC LIMIT 1`,
		id, OriginUser))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, nil
	}
	return v, err
}

func askPrompt(name string, n int, selection, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user asks about artifact %s, version %d.", name, n)
	if sel := strings.TrimSpace(selection); sel != "" {
		if r := []rune(sel); len(r) > maxSelection {
			sel = string(r[:maxSelection]) + "…"
		}
		b.WriteString(" Selected text:\n")
		for _, line := range strings.Split(sel, "\n") {
			b.WriteString("> " + line + "\n")
		}
	} else {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Request: %s\nRead it with `trebi artifact get %s@%d` and save the change with `trebi artifact put %s --from <file> --base %d`.",
		text, name, n, name, n)
	return b.String()
}
