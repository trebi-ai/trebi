package tray

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/update"
)

func TestNewUpdateAlert(t *testing.T) {
	t.Parallel()
	rel := update.Release{Tag: "v2.0.0", DMGURL: "https://github.com/x/Trebi.dmg"}
	raw := errors.New("GET https://api.github.com/repos/x: status 404")
	tests := []struct {
		name        string
		plan        update.Plan
		err         error
		title, info string
		ok, cancel  string
	}{
		{"busy", update.Plan{}, update.ErrBusy, "Trebi", "An update waits for the runs to end.", "OK", ""},
		{"check", update.Plan{}, fmt.Errorf("%w: %w", update.ErrCheck, raw), "Trebi cannot check for updates now", "Try again later.", "OK", ""},
		{"other", update.Plan{}, raw, "Trebi cannot download the update now", "Try again later.", "OK", ""},
		{"none", update.Plan{Kind: update.PlanNone}, nil, "Trebi is up to date", "You have version 1.0.0.", "OK", ""},
		{"manual", update.Plan{Kind: update.PlanManual, Release: rel}, nil, "Version 2.0.0 is available", "Download the new version.", "Download", "Later"},
		{"ready", update.Plan{Kind: update.PlanReady, Release: rel}, nil, "Update ready", "Trebi installs version 2.0.0 when no run is active.", "OK", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NewUpdateAlert(tt.plan, tt.err, "1.0.0")
			want := UpdateAlert{Title: tt.title, Info: tt.info, OK: tt.ok, Cancel: tt.cancel}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
			text := strings.ToLower(got.Title + got.Info)
			for _, bad := range []string{"http", "github", "404"} {
				if strings.Contains(text, bad) {
					t.Errorf("text %q has %q", text, bad)
				}
			}
		})
	}
}
