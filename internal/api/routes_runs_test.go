package api

import "testing"

// `trebi agent-hook` wraps the harness body in a payload string, so an
// approval opened from a waiting hook must still carry the question.
func TestHookQuestionReadsWrappedPayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"wrapped", `{"payload":"{\"question\":\"delete prod?\"}"}`, "delete prod?"},
		{"bare", `{"prompt":"delete prod?"}`, "delete prod?"},
		{"wrapped prose", `{"payload":"not json"}`, ""},
		{"empty", `{}`, ""},
		{"garbage", `nope`, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hookQuestion([]byte(tt.body)); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}
