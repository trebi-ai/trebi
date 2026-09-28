package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/trebi-ai/trebi/internal/notify"
)

// globalRoute sets only the global layer.
type globalRoute []notify.Step

func (globalRoute) JobLayer(context.Context, string, string) ([]notify.Step, bool, []string, string) {
	return nil, false, nil, ""
}

func (globalRoute) AgentLayer(context.Context, string, string) ([]notify.Step, bool, []string) {
	return nil, false, nil
}

func (globalRoute) WorkspaceLayer(context.Context, string) ([]notify.Step, bool, []string) {
	return nil, false, nil
}

func (g globalRoute) Global() ([]notify.Step, bool, []string) {
	return g, true, []string{"ops@example.com"}
}

type noChannels struct{}

func (noChannels) HasChannel(string) (bool, bool) { return false, false }

// TestNotifyRoutes: route shows the problems; test skips each problem step
// and tests only the asked step.
func TestNotifyRoutes(t *testing.T) {
	s, base := startTestServer(t)
	if code, _, _ := doJSON(t, http.MethodGet, base+"/v1/notify/route", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("unbound route: %d", code)
	}
	s.BindRouter(notify.NewResolver(globalRoute{{Via: "whatsapp", To: "+1555"}, {Via: "email"}}, noChannels{}, nil))

	code, body, _ := doJSON(t, http.MethodGet, base+"/v1/notify/route", nil)
	steps, _ := body["steps"].([]any)
	if code != http.StatusOK || len(steps) != 2 || body["source"] != "global" {
		t.Fatalf("route %d %v", code, body)
	}
	first, _ := steps[0].(map[string]any)
	second, _ := steps[1].(map[string]any)
	if first["problem"] != notify.ProblemNoConn || second["problem"] != notify.ProblemEmail {
		t.Fatalf("problems %v %v", first, second)
	}

	code, body, _ = doJSON(t, http.MethodPost, base+"/v1/notify/test", map[string]any{"step": 1})
	results, _ := body["results"].([]any)
	if code != http.StatusOK || len(results) != 1 {
		t.Fatalf("test %d %v", code, body)
	}
	if r, _ := results[0].(map[string]any); r["status"] != "skipped" || r["error"] != notify.ProblemEmail {
		t.Fatalf("result %v", r)
	}
	if code, _, _ := doJSON(t, http.MethodPost, base+"/v1/notify/test", map[string]any{"step": 5}); code != http.StatusBadRequest {
		t.Fatalf("out of range step: %d", code)
	}
}
