package api

import (
	"net/http/httptest"
	"testing"
)

func TestCheckWSOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{name: "no origin", origin: "", host: "127.0.0.1:7433", want: true},
		{name: "loopback ipv4", origin: "http://127.0.0.1:7433", host: "127.0.0.1:7433", want: true},
		{name: "localhost", origin: "http://localhost:7433", host: "127.0.0.1:7433", want: true},
		{name: "loopback ipv6", origin: "http://[::1]:7433", host: "[::1]:7433", want: true},
		{name: "same origin lan", origin: "http://192.168.8.150:7433", host: "192.168.8.150:7433", want: true},
		{name: "same origin tailscale", origin: "http://100.64.1.2:7433", host: "100.64.1.2:7433", want: true},
		{name: "cross site", origin: "http://evil.example.com", host: "192.168.8.150:7433", want: false},
		{name: "scheme mismatch still same host", origin: "https://192.168.8.150:7433", host: "192.168.8.150:7433", want: true},
		{name: "phone proxy", origin: "http://127.0.0.1:43123", host: "127.0.0.1:43123", want: true},
		{name: "malformed", origin: "://bad", host: "127.0.0.1:7433", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+tc.host+"/v1/runs/01X/attach", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := checkWSOrigin(r); got != tc.want {
				t.Fatalf("origin %q host %q: got %v want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}
