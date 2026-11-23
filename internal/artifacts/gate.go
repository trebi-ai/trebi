package artifacts

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

// Marker sets the outcome note of a run.
type Marker interface {
	Mark(ctx context.Context, opts runs.MarkOpts) (*runs.Run, error)
}

// Alerter raises and resolves inbox items.
type Alerter interface {
	Raise(ctx context.Context, opts alerts.RaiseOpts) (string, error)
	ResolveSubject(ctx context.Context, subject, how string) error
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

func (g *RunGate) PromptHint(context.Context, runs.LaunchOpts) string { return "" }

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
			Kind: alerts.KindArtifactMissing, Body: strings.TrimSpace("The run ended, but it did not save a new version of its report. " + note), RunID: run.ID,
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

// Continuer reaches the conversations of the artifact chat.
type Continuer interface {
	ConversationExists(ctx context.Context, convID string) bool
	NewConversation(ctx context.Context, workspace, operatorID, title string) (string, error)
	AppendSystem(ctx context.Context, convID, text string) error
	// SendToConversation sends text as a UI message of workspace (C4).
	SendToConversation(ctx context.Context, workspace, convID, text string) error
}

// WithContinuer wires Chat and Ask.
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

// AskResult names the chat that got the request.
type AskResult struct {
	Via            string `json:"via"` // conversation
	ConversationID string `json:"conversation_id"`
}

// ChatResult is the chat of an artifact. Created is true for a new
// conversation.
type ChatResult struct {
	ConversationID string `json:"conversation_id"`
	Created        bool   `json:"created"`
}

const maxSelection = 2000

// Ask sends the request of the user to the chat of the artifact (C3).
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
	n := cmp.Or(o.Version, a.LatestVersion)
	chat, err := s.chat(ctx, &a, n)
	if err != nil {
		return AskResult{}, err
	}
	res := AskResult{Via: OriginConversation, ConversationID: chat.ConversationID}
	return res, s.cont.SendToConversation(ctx, a.Workspace, chat.ConversationID, askPrompt(a.Name, n, o.Selection, text))
}

// Chat returns the one chat of an artifact (C1, C2). version 0 is the
// newest version.
func (s *Service) Chat(ctx context.Context, id string, version int) (ChatResult, error) {
	if s.cont == nil {
		return ChatResult{}, errors.New("chat is not wired")
	}
	a, err := s.Get(ctx, id)
	if err != nil {
		return ChatResult{}, err
	}
	return s.chat(ctx, &a, cmp.Or(version, a.LatestVersion))
}

func (s *Service) chat(ctx context.Context, a *Artifact, n int) (ChatResult, error) {
	if _, err := s.Version(ctx, a.ID, n); err != nil {
		return ChatResult{}, err
	}
	if a.ConversationID != "" && s.cont.ConversationExists(ctx, a.ConversationID) {
		return ChatResult{ConversationID: a.ConversationID}, s.chatContext(ctx, a, a.ConversationID, n)
	}
	res, err := s.findChat(ctx, a)
	if err != nil {
		return ChatResult{}, err
	}
	if res.ConversationID == "" {
		operator := ""
		if a.OwnerKind == OwnerAgent {
			operator = a.OwnerID
		}
		if res.ConversationID, err = s.cont.NewConversation(ctx, a.Workspace, operator, "About "+a.Title); err != nil {
			return ChatResult{}, err
		}
		res.Created = true
	}
	stored, err := s.storeChat(ctx, a, res.ConversationID)
	if err != nil {
		return ChatResult{}, err
	}
	if stored != res.ConversationID {
		return ChatResult{ConversationID: stored}, nil // a concurrent call stored its chat first
	}
	a.ConversationID, a.chatVersion = stored, 0
	return res, s.chatContext(ctx, a, stored, n)
}

// findChat finds the conversation of the owner from the versions (C2 steps
// 2 and 3). It is empty when no version came from a live conversation.
func (s *Service) findChat(ctx context.Context, a *Artifact) (ChatResult, error) {
	var convs []string
	if err := s.ids(ctx, &convs, `SELECT conversation_id FROM artifact_versions
  WHERE artifact_id=? AND origin_kind<>? AND conversation_id<>'' ORDER BY n DESC`, a.ID, OriginUser); err != nil {
		return ChatResult{}, err
	}
	for _, c := range convs {
		if s.cont.ConversationExists(ctx, c) {
			return ChatResult{ConversationID: c}, nil
		}
	}
	if s.runs == nil {
		return ChatResult{}, nil
	}
	var runIDs []string
	if err := s.ids(ctx, &runIDs, `SELECT run_id FROM artifact_versions
  WHERE artifact_id=? AND origin_kind<>? AND run_id<>'' ORDER BY n DESC`, a.ID, OriginUser); err != nil {
		return ChatResult{}, err
	}
	for _, id := range runIDs {
		run, err := s.runs.Get(ctx, id)
		if err == nil && run.ConversationID != "" && s.cont.ConversationExists(ctx, run.ConversationID) {
			return ChatResult{ConversationID: run.ConversationID}, nil
		}
	}
	return ChatResult{}, nil
}

// storeChat sets the chat of a when the stored chat is still the one that a
// holds. It returns the chat that the row holds after the write.
func (s *Service) storeChat(ctx context.Context, a *Artifact, convID string) (string, error) {
	next := *a
	next.Revision++
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE artifacts SET conversation_id=?, chat_version=0, revision=revision+1
  WHERE id=? AND conversation_id=?`, convID, a.ID, a.ConversationID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errChatTaken
		}
		return nil
	}, s.chatEvent(&next, convID))
	if errors.Is(err, errChatTaken) {
		cur, gerr := s.Get(ctx, a.ID)
		return cur.ConversationID, gerr
	}
	return convID, err
}

var errChatTaken = errors.New("artifact chat is taken")

// chatContext tells the chat which version is current (C5). It writes the
// line once per version.
func (s *Service) chatContext(ctx context.Context, a *Artifact, convID string, n int) error {
	if n <= a.chatVersion {
		return nil
	}
	line := fmt.Sprintf("This chat is about artifact %s (%s), version %d. Read it with `trebi artifact get %s@%d`. "+
		"Save a change with `trebi artifact put %s --from <file> --base %d`.", a.Title, a.Name, n, a.Name, n, a.Name, n)
	if err := s.cont.AppendSystem(ctx, convID, line); err != nil {
		return err
	}
	_, err := s.st.DB().ExecContext(ctx, `UPDATE artifacts SET chat_version=? WHERE id=? AND chat_version<?`, n, a.ID, n)
	a.chatVersion = n
	return err
}

func (s *Service) chatEvent(a *Artifact, convID string) store.BusEvent {
	ev := s.event("artifact.updated", "updated", a, a.LatestVersion)
	attrs, _ := ev.Attrs.(map[string]any)
	attrs["conversation_id"] = convID
	return ev
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
