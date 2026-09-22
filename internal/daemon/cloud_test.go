package daemon

import (
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/entitle"
)

// TestEmailTransportMode pins the exclusive selection (plan 2026-09-22
// Part L): the Worker URL wins and keeps the bearer path running, a
// hosted link needs the grant and the offline switch, and both configured
// reports the conflict without stopping case 1.
func TestEmailTransportMode(t *testing.T) {
	t.Parallel()
	off := false
	granted := entitle.State{Linked: true, Plan: "pro_cloud",
		Features: []entitle.Feature{entitle.Watchdog, entitle.HostedIngress}}
	free := entitle.State{Linked: true, Plan: "free", Features: []entitle.Feature{entitle.Watchdog}}
	worker := config.EmailConfig{Worker: "https://mail.example.workers.dev"}

	cases := []struct {
		name     string
		cfg      *config.Config
		ent      entitle.State
		want     emailMode
		conflict bool
	}{
		{"nothing configured", &config.Config{}, entitle.State{}, emailOff, false},
		{"link without the grant", &config.Config{}, free, emailOff, false},
		{"grant without the link", &config.Config{}, entitle.State{
			Features: []entitle.Feature{entitle.HostedIngress}}, emailOff, false},
		{"hosted with the grant", &config.Config{}, granted, emailHosted, false},
		{"hosted needs cloud enabled", &config.Config{
			Cloud: config.CloudConfig{Enabled: &off}}, granted, emailOff, false},
		{"bearer alone", &config.Config{Email: worker}, entitle.State{}, emailBearer, false},
		{"worker wins over hosted", &config.Config{Email: worker}, granted, emailBearer, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, conflict := emailTransportMode(tc.cfg, tc.ent)
			if got != tc.want || conflict != tc.conflict {
				t.Fatalf("mode = %v conflict = %v; want %v %v", got, conflict, tc.want, tc.conflict)
			}
		})
	}
}
