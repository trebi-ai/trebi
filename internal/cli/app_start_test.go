package cli

import (
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/trebi-ai/trebi/internal/tray"
)

// startLog records the calls of the fakes in order.
type startLog struct {
	calls       []string
	installed   bool
	running     bool
	hasShell    bool
	installErr  error
	refreshErr  error
	progressErr error
}

type fakeAppService struct{ l *startLog }

func (f fakeAppService) Installed() bool   { return f.l.installed }
func (f fakeAppService) TrayRunning() bool { return f.l.running }
func (f fakeAppService) Install(o platform.InstallOpts) error {
	f.l.calls = append(f.l.calls, "install "+o.Bin)
	return f.l.installErr
}
func (f fakeAppService) RefreshPath() (bool, error) {
	f.l.calls = append(f.l.calls, "refresh path")
	return true, f.l.refreshErr
}
func (f fakeAppService) Start() error {
	f.l.calls = append(f.l.calls, "start")
	return nil
}

type fakeAppShell struct{ l *startLog }

func (f fakeAppShell) Available() bool { return f.l.hasShell }
func (f fakeAppShell) Open(base, path string) error {
	f.l.calls = append(f.l.calls, "open "+base+path)
	return nil
}
func (f fakeAppShell) Progress(s tray.StartStep) error {
	f.l.calls = append(f.l.calls, "progress "+string(s))
	return f.l.progressErr
}

func TestAppStart(t *testing.T) {
	t.Parallel()
	base := "http://127.0.0.1:7433"
	tests := []struct {
		name string
		l    startLog
		want []string
		err  bool
	}{
		{
			name: "fresh install shows the steps in order",
			l:    startLog{hasShell: true},
			want: []string{"open " + base + "/welcome", "progress install", "install /app/Trebi.app/Contents/MacOS/trebi", "progress engine", "start", "progress setup"},
		},
		{
			name: "fresh install without a shell only installs and starts",
			l:    startLog{},
			want: []string{"install /app/Trebi.app/Contents/MacOS/trebi", "start"},
		},
		{
			name: "a shell error does not stop the install",
			l:    startLog{hasShell: true, progressErr: errors.New("no window")},
			want: []string{"open " + base + "/welcome", "progress install", "install /app/Trebi.app/Contents/MacOS/trebi", "progress engine", "start", "progress setup"},
		},
		{
			name: "install failure stops before the engine step",
			l:    startLog{hasShell: true, installErr: errors.New("denied")},
			want: []string{"open " + base + "/welcome", "progress install", "install /app/Trebi.app/Contents/MacOS/trebi"},
			err:  true,
		},
		{
			name: "installed with a running tray opens the home page",
			l:    startLog{installed: true, running: true, hasShell: true},
			want: []string{"refresh path", "start", "open " + base + "/"},
		},
		{
			name: "a path refresh error does not stop the start",
			l:    startLog{installed: true, refreshErr: errors.New("denied")},
			want: []string{"refresh path", "start"},
		},
		{
			name: "installed with a stopped tray reports no progress and opens no window",
			l:    startLog{installed: true, hasShell: true},
			want: []string{"refresh path", "start"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := tt.l
			a := appStart{
				exe:   "/app/Trebi.app/Contents/MacOS/trebi",
				svc:   fakeAppService{&l},
				shell: fakeAppShell{&l},
				url:   func() string { return base },
				log:   slog.Default(),
			}
			if err := a.run(); (err != nil) != tt.err {
				t.Fatalf("err %v, want err %v", err, tt.err)
			}
			if !reflect.DeepEqual(l.calls, tt.want) {
				t.Errorf("calls\n got %q\nwant %q", l.calls, tt.want)
			}
		})
	}
}
