package runs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/uploads"
)

// recordSession records each prompt. refuse makes a prompt with images fail
// as unsupported.
type recordSession struct {
	*stubCLISession
	refuse  bool
	prompts []agentwire.Prompt
}

func (s *recordSession) Prompt(_ context.Context, p agentwire.Prompt) error {
	if s.refuse && len(p.Attachments) > 0 {
		return agentwire.ErrUnsupported
	}
	s.prompts = append(s.prompts, p)
	return nil
}

func TestPromptCLIAttachments(t *testing.T) {
	atts := uploads.Attachments{{ID: "upl_1", MIME: "image/png", Path: "/h/uploads/ws/upl_1.png"}}
	cases := []struct {
		name     string
		harness  string
		refuse   bool
		wantText string
		wantWire bool
	}{
		{name: "claude takes images", harness: "claude", wantText: "look", wantWire: true},
		{name: "pi gets paths", harness: tmux.Pi, wantText: "look\n[Image: /h/uploads/ws/upl_1.png]"},
		{name: "refused falls back to paths", harness: "codex", refuse: true, wantText: "look\n[Image: /h/uploads/ws/upl_1.png]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, run, cr := cliReaperFixture(t, CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j"})
			sess := &recordSession{stubCLISession: newStubCLISession(), refuse: tc.refuse}
			cr.sess, cr.harness = sess, tc.harness
			ctx := context.Background()
			if _, err := svc.promptCLI(ctx, run.ID, cr, agentwire.Prompt{Text: "look"}, atts, OriginMessage); err != nil {
				t.Fatal(err)
			}
			if len(sess.prompts) != 1 {
				t.Fatalf("prompts = %+v", sess.prompts)
			}
			p := sess.prompts[0]
			if p.Text != tc.wantText || (len(p.Attachments) > 0) != tc.wantWire {
				t.Fatalf("prompt = %+v", p)
			}
			if tc.wantWire && (p.Attachments[0].Path != atts[0].Path || p.Attachments[0].MIME != "image/png") {
				t.Fatalf("attachment = %+v", p.Attachments[0])
			}
			evs, err := svc.EventsSince(ctx, run.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			var user map[string]string
			for _, e := range evs {
				if e.Kind == "user" {
					_ = json.Unmarshal(e.Detail, &user)
				}
			}
			if user["text"] != "look" || user["attachments"] != "upl_1" {
				t.Fatalf("user event = %v", user)
			}
		})
	}
}

func TestPromptCLIImageOnly(t *testing.T) {
	svc, run, cr := cliReaperFixture(t, CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j"})
	sess := &recordSession{stubCLISession: newStubCLISession()}
	cr.sess, cr.harness = sess, tmux.Pi
	atts := uploads.Attachments{{ID: "upl_1", MIME: "image/png", Path: "/a.png"}}
	if _, err := svc.promptCLI(context.Background(), run.ID, cr, agentwire.Prompt{}, atts, OriginMessage); err != nil {
		t.Fatal(err)
	}
	if got := sess.prompts[0].Text; !strings.HasPrefix(got, "[Image: /a.png]") {
		t.Fatalf("text = %q", got)
	}
}
