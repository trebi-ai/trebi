package runs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestEveryDeclaredEnumConstantIsValid is the D12 drift guard: a new constant
// added to run.go without a matching validator/classifier update fails here.
func TestEveryDeclaredEnumConstantIsValid(t *testing.T) {
	for _, s := range declaredConstants(t, "Status") {
		if !ValidStatus(s) {
			t.Errorf("Status %q not accepted by ValidStatus", s)
		}
		st := Status(s)
		if st.IsLive() == st.IsTerminal() {
			t.Errorf("Status %q must be exactly one of live or terminal", s)
		}
	}
	for _, s := range declaredConstants(t, "EndReason") {
		if !ValidEndReason(s) {
			t.Errorf("EndReason %q not accepted by ValidEndReason", s)
		}
	}
}

// TestDeriveJobDisplayStatus pins the §3.2 derivation table with precedence.
func TestDeriveJobDisplayStatus(t *testing.T) {
	cases := []struct {
		name     string
		archived bool
		active   bool
		live     int
		waiting  bool
		last     Status
		want     JobDisplayStatus
	}{
		{"paused wins over live", false, false, 1, true, StatusComplete, JobDisplayPaused},
		{"waiting wins over running", false, true, 2, true, StatusFailed, JobDisplayWaiting},
		{"running when live", false, true, 1, false, StatusFailed, JobDisplayRunning},
		{"complete maps to ok", false, true, 0, false, StatusComplete, JobDisplayOK},
		{"failed maps to failed", false, true, 0, false, StatusFailed, JobDisplayFailed},
		{"timed_out maps to failed", false, true, 0, false, StatusTimedOut, JobDisplayFailed},
		{"skipped maps to idle", false, true, 0, false, StatusSkipped, JobDisplayIdle},
		{"never ran is idle", false, true, 0, false, Status(""), JobDisplayIdle},
		{"archived with no live run", true, true, 0, false, StatusComplete, JobDisplayArchived},
		{"archived with a live run", true, true, 1, false, StatusComplete, JobDisplayRunning},
		{"archived with a waiting run", true, true, 1, true, StatusComplete, JobDisplayWaiting},
		{"archived wins over paused", true, false, 0, false, StatusComplete, JobDisplayArchived},
		{"live archived wins over paused", true, false, 1, false, StatusComplete, JobDisplayRunning},
	}
	for _, c := range cases {
		got := DeriveJobDisplayStatus(c.archived, c.active, c.live, c.waiting, c.last)
		if got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// TestValidatorsRejectUnknown pins the closed-set property.
func TestValidatorsRejectUnknown(t *testing.T) {
	if ValidStatus("bogus") {
		t.Error("ValidStatus accepted bogus")
	}
	if ValidEndReason("bogus") {
		t.Error("ValidEndReason accepted bogus")
	}
}

func TestStatusHelpers(t *testing.T) {
	t.Parallel()
	if !StatusRunning.IsLive() {
		t.Fatal("running is live")
	}
	if StatusComplete.IsLive() {
		t.Fatal("complete is not live")
	}
	if !StatusParked.IsTerminal() || ValidStatus("resumed") {
		t.Fatal("parked is terminal; resumed is no status")
	}
	if !ValidStatus("complete") || ValidStatus("bogus") {
		t.Fatal("ValidStatus mismatch")
	}
	if !ValidEndReason("sentinel") || ValidEndReason("nope") {
		t.Fatal("ValidEndReason mismatch")
	}
}

// TestOutcomeStatus pins the human vocabulary: success is the UI word for
// complete; running/resumed can never be an outcome.
func TestOutcomeStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]Status{
		"success": StatusComplete, "Success": StatusComplete, "complete": StatusComplete,
		"failed": StatusFailed, "skipped": StatusSkipped,
		"running": "", "resumed": "", "": "", "bogus": "",
	}
	for word, want := range cases {
		if got := OutcomeStatus(word); got != want {
			t.Errorf("OutcomeStatus(%q)=%q want %q", word, got, want)
		}
	}
	for _, s := range []Status{StatusComplete, StatusFailed, StatusSkipped} {
		if !ValidOutcomeStatus(s) {
			t.Errorf("ValidOutcomeStatus(%q)=false", s)
		}
	}
	for _, s := range []Status{StatusRunning, StatusExited, StatusTimedOut} {
		if ValidOutcomeStatus(s) {
			t.Errorf("ValidOutcomeStatus(%q)=true", s)
		}
	}
}

// declaredConstants parses run.go and returns the string values of all
// constants declared with the given type name.
func declaredConstants(t *testing.T, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != typeName {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no constants found for type %s", typeName)
	}
	return out
}
