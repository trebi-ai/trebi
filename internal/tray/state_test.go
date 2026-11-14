package tray

import (
	"reflect"
	"testing"
)

func TestSnapshotMenu(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		snap    Snapshot
		texts   []string
		actions map[string]Action
		title   string
	}{
		{
			name: "running",
			snap: Snapshot{Status: StatusRunning, RunningCount: 2, WaitingCount: 1},
			texts: []string{
				"Trebi is running", "2 runs active · 1 waiting for you", "", "Open Trebi", "",
				"Restart Service", "Stop Service", "", "Open Log", "Show Trebi Folder", "", "Check for updates…", "About Trebi", "Quit Trebi",
			},
			actions: map[string]Action{"Open Trebi": ActionOpenBrowser, "Restart Service": ActionRestart, "Stop Service": ActionStop, "Quit Trebi": ActionQuit, "Check for updates…": ActionCheckUpdates},
			title:   "1",
		},
		{
			name:    "running idle and draining",
			snap:    Snapshot{Status: StatusRunning, Draining: true},
			texts:   []string{"Trebi is running · new runs paused", "No runs active", "", "Open Trebi", "", "Restart Service", "Stop Service", "", "Open Log", "Show Trebi Folder", "", "Check for updates…", "About Trebi", "Quit Trebi"},
			actions: map[string]Action{"Open Trebi": ActionOpenBrowser},
		},
		{
			name: "running with shell",
			snap: Snapshot{Status: StatusRunning, HasShell: true},
			texts: []string{
				"Trebi is running", "No runs active", "", "Open Trebi", "Open Trebi in Browser", "",
				"Restart Service", "Stop Service", "", "Open Log", "Show Trebi Folder", "", "Check for updates…", "About Trebi", "Quit Trebi",
			},
			actions: map[string]Action{"Open Trebi": ActionOpenApp, "Open Trebi in Browser": ActionOpenBrowser},
		},
		{
			name:    "stopped with shell",
			snap:    Snapshot{Status: StatusStopped, HasShell: true},
			texts:   []string{"Trebi is stopped", "", "Open Trebi", "Open Trebi in Browser", "", "Start Service", "", "Open Log", "Show Trebi Folder", "", "Check for updates…", "About Trebi", "Quit Trebi"},
			actions: map[string]Action{"Open Trebi": ActionNone, "Open Trebi in Browser": ActionNone},
		},
		{
			name:    "stopped",
			snap:    Snapshot{Status: StatusStopped, WaitingCount: 3},
			texts:   []string{"Trebi is stopped", "", "Open Trebi", "", "Start Service", "", "Open Log", "Show Trebi Folder", "", "Check for updates…", "About Trebi", "Quit Trebi"},
			actions: map[string]Action{"Open Trebi": ActionNone, "Start Service": ActionStart, "Trebi is stopped": ActionNone},
		},
		{
			name:    "not responding",
			snap:    Snapshot{Status: StatusNotResponding},
			texts:   []string{"Trebi is not responding", "", "Open Trebi", "", "Restart Service", "Stop Service", "", "Open Log", "Show Trebi Folder", "", "Check for updates…", "About Trebi", "Quit Trebi"},
			actions: map[string]Action{"Open Trebi": ActionNone, "Restart Service": ActionRestart},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items := tt.snap.Menu()
			texts := make([]string, len(items))
			byText := map[string]Action{}
			for i, it := range items {
				texts[i] = it.Text
				byText[it.Text] = it.Action
			}
			if !reflect.DeepEqual(texts, tt.texts) {
				t.Fatalf("menu:\n got %q\nwant %q", texts, tt.texts)
			}
			for text, want := range tt.actions {
				if got := byText[text]; got != want {
					t.Errorf("%q: action %d, want %d", text, got, want)
				}
			}
			if got := tt.snap.Title(); got != tt.title {
				t.Errorf("title %q, want %q", got, tt.title)
			}
		})
	}
}

func TestSnapshotAbout(t *testing.T) {
	t.Parallel()
	got := Snapshot{Version: "0.2.80", EditionVersion: "4d1c96e", UIVersion: "0.1.25"}.about()
	if want := "Version 0.2.80\nEdition 4d1c96e\nUI 0.1.25"; got != want {
		t.Fatalf("about %q, want %q", got, want)
	}
	if got := (Snapshot{}).about(); got != "Version unknown" {
		t.Fatalf("empty about %q", got)
	}
}
