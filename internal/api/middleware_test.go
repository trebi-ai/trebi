package api

import (
	"net/http"
	"testing"
)

func TestAllowQueryTokenLoginAttach(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/v1/harnesses/claude/login/attach", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !allowQueryToken(req) {
		t.Fatal("expected query token on harness login attach")
	}

	poll, err := http.NewRequest(http.MethodGet, "/v1/harnesses/claude/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	if allowQueryToken(poll) {
		t.Fatal("did not expect query token on login poll")
	}

	run, err := http.NewRequest(http.MethodGet, "/v1/runs/run_1/attach", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !allowQueryToken(run) {
		t.Fatal("expected query token on run attach")
	}
}
