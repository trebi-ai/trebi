package requests

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var jsonBlock = regexp.MustCompile("(?s)```json\n(.*?)```\n")

type promptCase struct {
	name    string
	reqs    []Request
	capsule bool
}

var promptAt = time.Date(2026, 9, 28, 14, 5, 0, 0, time.UTC)

// promptAnswer is one form answer and who gave it.
type promptAnswer struct {
	FormAnswer
	By, Via string
}

func promptCases() []promptCase {
	draft := &Draft{Format: "markdown", Text: "Hi Ana,\n\nThe invoice is attached.\n"}
	editText := "Hi Ana,\n\nUse `trebi` and \"quotes\" <html> & more. \nLine two.\n"
	opts := []Option{{ID: "a", Label: "Plan A"}, {ID: "b", Label: "Plan B", Detail: "slower"}, {ID: "c", Label: "Plan C"}}
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"count":{"type":"integer"},"ok":{"type":"boolean"},"notes":{"type":"string","format":"multiline"}}}`)
	user := func(d Decision) *promptAnswer {
		return &promptAnswer{FormAnswer: FormAnswer{Decision: d}, By: ByUser, Via: ViaUI}
	}
	with := func(a *promptAnswer, f func(*promptAnswer)) *promptAnswer { f(a); return a }
	three := withForms(
		promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, with(user(DecisionApprove), func(a *promptAnswer) { a.Note = "Keep the tag." })),
		Form{Type: TypeChoose, Title: "Pick a plan", Payload: Payload{Options: opts}, Answer: &FormAnswer{Decision: DecisionChoose, Choices: []string{"b"}}},
		Form{Type: TypeReview, Title: "Send the reply to Ana", Payload: Payload{Draft: draft}, Answer: &FormAnswer{Decision: DecisionEdit, Text: editText}},
	)
	threeLong := withForms(
		promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, user(DecisionApprove)),
		Form{Type: TypeChoose, Title: "Pick a plan", Payload: Payload{Options: opts}, Answer: &FormAnswer{Decision: DecisionChoose, Choices: []string{"b"}}},
		Form{Type: TypeReview, Title: "Send the reply to Ana", Payload: Payload{Draft: draft}, Answer: &FormAnswer{Decision: DecisionEdit, Text: strings.Repeat("x", 40<<10)}},
	)
	return []promptCase{
		{name: "confirm-approve", reqs: []Request{promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, user(DecisionApprove))}},
		{name: "confirm-approve-note", reqs: []Request{promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, with(user(DecisionApprove), func(a *promptAnswer) { a.Note = "Keep the tag." }))}},
		{name: "confirm-reject", reqs: []Request{promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, with(user(DecisionReject), func(a *promptAnswer) { a.Note = "Not this week." }))}},
		{name: "review-approve", reqs: []Request{promptReq("R01", TypeReview, "Send the reply to Ana", Payload{Draft: draft}, user(DecisionApprove))}},
		{name: "review-edit", reqs: []Request{promptReq("R01", TypeReview, "Send the reply to Ana", Payload{Draft: draft}, with(user(DecisionEdit), func(a *promptAnswer) { a.Text = editText }))}},
		{name: "review-edit-unicode", reqs: []Request{promptReq("R01", TypeReview, "Send the reply to Ana", Payload{Draft: draft}, with(user(DecisionEdit), func(a *promptAnswer) { a.Text = "Merci 🎉 — שלום עולם\n"; a.Via = ViaEmail }))}},
		{name: "review-reject", reqs: []Request{promptReq("R01", TypeReview, "Send the reply to Ana", Payload{Draft: draft}, user(DecisionReject))}},
		{name: "respond", reqs: []Request{promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, with(user(DecisionRespond), func(a *promptAnswer) {
			a.Message = "First check who uses it.\n\nThen ask me again."
			a.By, a.Via = ByAssistant, ViaAsst
		}))}},
		{name: "choose-one", reqs: []Request{promptReq("R01", TypeChoose, "Pick a plan", Payload{Options: opts}, with(user(DecisionChoose), func(a *promptAnswer) { a.Choices = []string{"b"}; a.Via = ViaCLI }))}},
		{name: "choose-multi", reqs: []Request{promptReq("R01", TypeChoose, "Pick plans", Payload{Options: opts, Multi: true}, with(user(DecisionChoose), func(a *promptAnswer) { a.Choices = []string{"a", "c"} }))}},
		{name: "form-submit", reqs: []Request{promptReq("R01", TypeForm, "Fill in the order", Payload{Schema: schema}, with(user(DecisionSubmit), func(a *promptAnswer) {
			a.Values = json.RawMessage(`{"name": "Ana", "count": 3, "ok": true, "notes": "line one\nline two"}`)
		}))}},
		{name: "expired", reqs: []Request{promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, nil)}},
		{name: "two-requests", reqs: []Request{
			promptReq("R01", TypeConfirm, "Delete the old branch", Payload{}, user(DecisionApprove)),
			promptReq("R02", TypeChoose, "Pick a plan", Payload{Options: opts}, with(user(DecisionChoose), func(a *promptAnswer) { a.Choices = []string{"a"} })),
		}},
		{name: "truncated", reqs: []Request{promptReq("R01", TypeReview, "Send the reply to Ana", Payload{Draft: draft}, with(user(DecisionEdit), func(a *promptAnswer) { a.Text = strings.Repeat("x", 40<<10) }))}},
		{name: "three-forms", reqs: []Request{three}},
		{name: "three-forms-truncated", reqs: []Request{threeLong}},
		{name: "capsule-review-edit", capsule: true, reqs: []Request{promptReq("R01", TypeReview, "Send the reply to Ana", Payload{Draft: draft}, with(user(DecisionEdit), func(a *promptAnswer) { a.Text = editText }))}},
	}
}

func (c promptCase) render() string {
	d := Delivery{Run: &runs.Run{ID: "RUN1"}, Requests: c.reqs}
	if c.capsule {
		return d.CapsulePrompt()
	}
	return d.Prompt()
}

func TestResumePrompt(t *testing.T) {
	t.Parallel()
	for _, c := range promptCases() {
		got := c.render()
		path := filepath.Join("testdata", "prompts", c.name+".golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with -update)", c.name, err)
		}
		if got != string(want) {
			t.Errorf("%s: prompt differs from %s\n--- got ---\n%s", c.name, path, got)
		}
	}
}

// TestResumePromptJSONRoundTrip proves that the model gets the exact answer
// data back from the fenced JSON of each golden.
func TestResumePromptJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, c := range promptCases() {
		data, err := os.ReadFile(filepath.Join("testdata", "prompts", c.name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		var answers []answerJSON
		for _, m := range jsonBlock.FindAllSubmatch(data, -1) {
			var j answerJSON
			if err := json.Unmarshal(m[1], &j); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if j.Decision != "" {
				answers = append(answers, j)
			}
		}
		var forms []Form
		var ids []string
		for _, r := range c.reqs {
			for _, f := range r.Forms {
				if r.Status != StatusAnswered || f.Answer == nil {
					f.Answer = &FormAnswer{Decision: DecisionExpired}
				}
				forms = append(forms, f)
				ids = append(ids, r.ID)
			}
		}
		if len(answers) != len(forms) {
			t.Fatalf("%s: %d answer blocks, want %d", c.name, len(answers), len(forms))
		}
		for i, f := range forms {
			got, a := answers[i], f.Answer
			if got.RequestID != ids[i] || got.Idx != f.Idx || got.Decision != a.Decision {
				t.Errorf("%s: block %d is %s/%d/%s", c.name, i, got.RequestID, got.Idx, got.Decision)
			}
			if got.Truncated {
				continue
			}
			if got.Text != a.Text || got.Message != a.Message || got.Note != a.Note || !reflect.DeepEqual(got.Choices, a.Choices) {
				t.Errorf("%s: block %d does not match the answer", c.name, i)
			}
			if len(a.Values) > 0 {
				var want, have any
				_ = json.Unmarshal(a.Values, &want)   // fixed test input
				_ = json.Unmarshal(got.Values, &have) // checked by DeepEqual
				if !reflect.DeepEqual(want, have) {
					t.Errorf("%s: values %s, want %s", c.name, got.Values, a.Values)
				}
			}
		}
	}
}

func TestPromptTruncates(t *testing.T) {
	t.Parallel()
	for _, c := range promptCases() {
		if c.name != "truncated" && c.name != "three-forms-truncated" {
			continue
		}
		got := c.render()
		if len(got) > maxPrompt || !strings.Contains(got, "trebi request get R01") {
			t.Fatalf("truncated prompt: %d bytes", len(got))
		}
	}
}

func promptReq(id string, t Type, title string, p Payload, a *promptAnswer) Request {
	r := Request{ID: id, RunID: "RUN1", Title: title, Status: StatusAnswered, Forms: []Form{{Type: t, Title: title, Payload: p}}}
	if a == nil || a.Decision == DecisionExpired {
		r.Status = StatusExpired
		r.Answered = &Answered{By: ViaDaemon, Via: ViaDaemon, At: promptAt}
		return r
	}
	r.Answered = &Answered{By: a.By, Via: a.Via, At: promptAt}
	fa := a.FormAnswer
	r.Forms[0].Answer = &fa
	return r
}

// withForms adds forms with their answers to r, in idx order.
func withForms(r Request, forms ...Form) Request {
	for _, f := range forms {
		f.Idx = len(r.Forms)
		r.Forms = append(r.Forms, f)
	}
	r.Title = "Ship the release"
	return r
}
