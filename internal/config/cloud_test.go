package config

import "testing"

func TestRemoteMCPOnDefault(t *testing.T) {
	t.Parallel()
	on, off := true, false
	for _, tc := range []struct {
		val  *bool
		want bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		if got := (CloudConfig{RemoteMCP: tc.val}).RemoteMCPOn(); got != tc.want {
			t.Errorf("RemoteMCPOn(%v) = %v, want %v", tc.val, got, tc.want)
		}
	}
}
