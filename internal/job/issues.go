package job

import (
	"fmt"
	"slices"
	"time"
)

// Severities of an issue.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityInfo    = "info"
)

// Issue codes. An error stops validate; a warning and an info do not.
const (
	CodeInputInvalid          = "input_invalid"
	CodeInputType             = "input_type"
	CodeInputUnknown          = "input_unknown"
	CodeTemplateInvalid       = "template_invalid"
	CodeEventTypeUnknown      = "event_type_unknown"
	CodeTriggerConflict       = "trigger_conflict"
	CodeReplyRoomTaken        = "reply_room_taken"
	CodeRequiredUnfilled      = "required_unfilled"
	CodeMayBeEmpty            = "may_be_empty"
	CodeFieldUnknown          = "field_unknown"
	CodePlaceholderUndeclared = "placeholder_undeclared"
	CodeInputUnused           = "input_unused"
	CodeTriggerAccessReadOnly = "trigger_access_read_only"
	CodeTriggerAccessNone     = "trigger_access_none"
	CodeArchivedAtInvalid     = "archived_at_invalid"
)

// Issue is one problem of a job file. Path names the key, for example
// inputs.tone.default or triggers[2].inputs.body.
type Issue struct {
	Path     string `json:"path"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// IssueList is the issues of one job.
type IssueList []Issue

// Err returns the first error issue.
func (l IssueList) Err() *Issue {
	for i := range l {
		if l[i].Severity == SeverityError {
			return &l[i]
		}
	}
	return nil
}

// Messages lists the messages of one severity.
func (l IssueList) Messages(severity string) []string {
	var out []string
	for _, is := range l {
		if is.Severity == severity {
			out = append(out, is.Message)
		}
	}
	return out
}

// Issues lists the problems of a parsed job that need no connection: the
// inputs block, the schedule keys, and the prompt placeholders. routed is
// true when a route names the job, so the prompt may use the event vars.
// The checks that read a connector manifest are in the connectors package.
func (j *Job) Issues(routed bool) IssueList {
	var out IssueList
	if _, err := time.Parse(time.RFC3339, j.ArchivedAt); j.ArchivedAt != "" && err != nil {
		out = append(out, Issue{Path: "archived_at", Code: CodeArchivedAtInvalid, Severity: SeverityError, Message: "archived_at must be an RFC3339 time"})
	}
	for _, in := range j.Inputs {
		out = append(out, in.faults...)
	}
	for i, tr := range j.Triggers {
		out = append(out, j.triggerIssues(fmt.Sprintf("triggers[%d]", i), tr, false)...)
	}
	out = append(out, j.promptIssues(routed)...)
	return out
}

// Warnings lists the warning messages of Issues.
func (j *Job) Warnings(routed bool) []string { return j.Issues(routed).Messages(SeverityWarning) }

// RouteIssues checks the event trigger that a route gives for j, under
// path: the conflicts, the inputs map, and the batch vars of the prompt.
func (j *Job) RouteIssues(path string, tr Trigger) IssueList {
	out := j.triggerIssues(path, tr, true)
	if tr.Batched() {
		out = append(out, j.batchIssues(path+".batch", tr)...)
	}
	return out
}

// triggerIssues checks the keys of one trigger and its inputs map.
func (j *Job) triggerIssues(path string, tr Trigger, routed bool) IssueList {
	var out IssueList
	add := func(p, code, sev, msg string) {
		out = append(out, Issue{Path: p, Code: code, Severity: sev, Message: msg})
	}
	if key, msg := tr.conflict(); key != "" {
		add(path+"."+key, CodeTriggerConflict, SeverityError, msg)
	}
	schedule := tr.Family != "event"
	list := j.InputList(routed)
	for _, mv := range tr.Inputs {
		p := path + ".inputs." + mv.Name
		in, ok := list.Get(mv.Name)
		var code, msg string
		switch {
		case !ok:
			code, msg = CodeInputUnknown, fmt.Sprintf("%q is not an input of this job", mv.Name)
		case !mv.IsTemplate():
			if _, err := in.Coerce(mv.Value); err != nil {
				code, msg = CodeInputInvalid, err.Error()
			}
		case schedule:
			code, msg = CodeTemplateInvalid, fmt.Sprintf("%s: a schedule has no event; use a fixed value", in.Title())
		case templateFault(mv.Value) != "":
			code, msg = CodeTemplateInvalid, fmt.Sprintf("%s: %s", in.Title(), templateFault(mv.Value))
		}
		if code != "" {
			add(p, code, SeverityError, msg)
		}
	}
	for _, in := range list {
		if _, ok := tr.Inputs.Get(in.Name); ok || !in.Required || in.Default != nil {
			continue
		}
		switch {
		case schedule:
			add(path+".inputs."+in.Name, CodeRequiredUnfilled, SeverityError,
				fmt.Sprintf("%s is required, but this schedule does not fill it; add a fixed value or a default", in.Title()))
		case tr.Manual:
			add(path+".inputs."+in.Name, CodeRequiredUnfilled, SeverityInfo,
				fmt.Sprintf("%s: the run form asks for it when the event does not fill it", in.Title()))
		}
	}
	return out
}

// promptIssues checks the placeholders of the prompt against the inputs
// block.
func (j *Job) promptIssues(routed bool) IssueList {
	var out IssueList
	if len(j.Inputs) == 0 {
		return out
	}
	keys := DiscoverInputKeys(j.Prompt)
	for _, k := range j.undeclaredPlaceholders(routed) {
		out = append(out, Issue{Path: "prompt", Code: CodePlaceholderUndeclared, Severity: SeverityWarning,
			Message: fmt.Sprintf("{{%s}} is not in inputs; the run form shows it as free text", k)})
	}
	for _, in := range j.Inputs {
		if !slices.Contains(keys, in.Name) {
			out = append(out, Issue{Path: "inputs." + in.Name, Code: CodeInputUnused, Severity: SeverityWarning,
				Message: fmt.Sprintf("the prompt does not use {{%s}}", in.Name)})
		}
	}
	return out
}

// batchIssues warns about the prompt vars that stay empty in a batch run.
func (j *Job) batchIssues(path string, tr Trigger) IssueList {
	var out IssueList
	for _, k := range DiscoverInputKeys(j.Prompt) {
		if singleEventVars[k] || (tr.Extract != "" && !batchVars[k]) {
			out = append(out, Issue{Path: path, Code: CodeMayBeEmpty, Severity: SeverityWarning,
				Message: fmt.Sprintf("{{%s}} is empty in a batch run; use {{events}} or {{events_file}}", k)})
		}
	}
	return out
}

// conflict returns the key and the message of a trigger_conflict issue.
func (t Trigger) conflict() (key, msg string) {
	schedule := t.Family != "event"
	switch {
	case schedule && t.Manual:
		return "manual", `"manual" applies only to an event trigger`
	case schedule && len(t.Types) > 0:
		return "types", `"types" applies only to an event trigger`
	case t.Manual && t.Batched():
		return "manual", `"manual" cannot be used with "batch"; a person starts one run for one event`
	case t.Manual && t.Continues():
		return "manual", `"manual" cannot be used with "replies: continue"`
	case t.Manual && t.Chat == ChatReply:
		return "manual", `"manual" cannot be used with "chat: reply"; a run that a person starts has no chat`
	}
	return "", ""
}
