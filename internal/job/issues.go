package job

import (
	"fmt"
	"slices"
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
// inputs block, the trigger keys, and the prompt placeholders. The checks
// that read a connector manifest are in the connectors package.
func (j *Job) Issues() IssueList {
	var out IssueList
	for _, in := range j.Inputs {
		out = append(out, in.faults...)
	}
	for i, tr := range j.Triggers {
		out = append(out, j.triggerIssues(i, tr)...)
	}
	out = append(out, j.promptIssues()...)
	return out
}

// Warnings lists the warning messages of Issues.
func (j *Job) Warnings() []string { return j.Issues().Messages(SeverityWarning) }

// triggerIssues checks the keys of trigger i and its inputs map.
func (j *Job) triggerIssues(i int, tr Trigger) IssueList {
	var out IssueList
	path := fmt.Sprintf("triggers[%d]", i)
	add := func(p, code, sev, msg string) {
		out = append(out, Issue{Path: p, Code: code, Severity: sev, Message: msg})
	}
	if key, msg := tr.conflict(); key != "" {
		add(path+"."+key, CodeTriggerConflict, SeverityError, msg)
	}
	schedule := tr.Family != "event"
	list := j.InputList()
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
// block and the batch vars.
func (j *Job) promptIssues() IssueList {
	var out IssueList
	keys := DiscoverInputKeys(j.Prompt)
	if len(j.Inputs) > 0 {
		for _, k := range j.undeclaredPlaceholders() {
			out = append(out, Issue{Path: "prompt", Code: CodePlaceholderUndeclared, Severity: SeverityWarning,
				Message: fmt.Sprintf("{{%s}} is not in inputs; the run form shows it as free text", k)})
		}
		for _, in := range j.Inputs {
			if !slices.Contains(keys, in.Name) {
				out = append(out, Issue{Path: "inputs." + in.Name, Code: CodeInputUnused, Severity: SeverityWarning,
					Message: fmt.Sprintf("the prompt does not use {{%s}}", in.Name)})
			}
		}
	}
	extract := false
	batched := false
	for _, tr := range j.Triggers {
		if tr.Batched() {
			batched = true
			extract = extract || tr.Extract != ""
		}
	}
	if !batched {
		return out
	}
	for _, k := range keys {
		if singleEventVars[k] || (extract && !batchVars[k]) {
			out = append(out, Issue{Path: "prompt", Code: CodeMayBeEmpty, Severity: SeverityWarning,
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
