//go:build darwin && cgo

package tray

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/caseymrm/menuet"
	"github.com/trebi-ai/trebi/internal/home"
)

//go:embed assets/idle.png
var idleIcon []byte

//go:embed assets/alert.png
var alertIcon []byte

// Available is true on macOS with cgo (menuet).
const Available = true

// Run starts the menu-bar tray and blocks (menuet owns the main thread).
// Icon presence means the trebi service process is alive.
// Quit (built-in or "Quit Trebi service") exits the process and stops the daemon.
// onQuit is invoked once when the user chooses quit (before process exit).
func Run(ctx context.Context, onQuit func()) error {
	st := NewState()
	st.Set(Snapshot{Message: "starting…"})

	app := menuet.App()
	app.Name = "trebi"
	app.Label = "ai.trebi.daemon"

	app.Children = func() []menuet.MenuItem {
		return buildMenu(st, onQuit)
	}

	idleURL, alertURL, stopIconServer := startIconServer(st.log)
	defer stopIconServer()
	setIdleState(app, idleURL)

	var once sync.Once
	quitOnce := func() {
		once.Do(func() {
			if onQuit != nil {
				onQuit()
			}
		})
	}

	go pollHealth(ctx, st, func() {
		snap := st.Get()
		if !snap.Healthy {
			setAlertState(app, alertURL)
		} else if snap.WaitingCount > 0 || snap.Draining {
			setAlertState(app, alertURL)
		} else {
			setIdleState(app, idleURL)
		}
		menuet.App().MenuChanged()
	})

	// If parent cancels (daemon died), quit the tray app.
	go func() {
		<-ctx.Done()
		quitOnce()
		// menuet has no public quit API; exit process so launchd sees clean stop.
		os.Exit(0)
	}()

	app.RunApplication()
	// RunApplication returns when user quits the menu bar app.
	quitOnce()
	return nil
}

func buildMenu(st *State, onQuit func()) []menuet.MenuItem {
	snap := st.Get()
	lines := snap.MenuLines()
	items := make([]menuet.MenuItem, 0, len(lines)+4)
	// Status header (non-clickable text rows except Quit).
	for i, line := range lines {
		if line == "Quit Trebi service" {
			continue
		}
		if i == 0 {
			items = append(items, menuet.MenuItem{Text: line})
			continue
		}
		items = append(items, menuet.MenuItem{Text: line})
	}

	items = append(items, menuet.MenuItem{Type: menuet.Separator})
	items = append(items, menuet.MenuItem{
		Text: "Open daemon log",
		Clicked: func() {
			logPath := home.DaemonLogPath()
			if err := exec.Command("open", "-a", "Console", logPath).Start(); err != nil {
				// Fallback: open in default app / reveal.
				if err2 := exec.Command("open", logPath).Start(); err2 != nil {
					st.log.Warn("tray: open log failed", "path", logPath, "error", err2)
				}
			}
		},
	})
	items = append(items, menuet.MenuItem{
		Text: "Quit Trebi service",
		Clicked: func() {
			if onQuit != nil {
				onQuit()
			}
			// Exit 0 so launchd KeepAlive OnFailure does not restart.
			os.Exit(0)
		},
	})
	// menuet also appends a system Quit — same effect (process exit).
	return items
}

func startIconServer(log *slog.Logger) (string, string, func()) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Warn("tray: icon server listen failed", "error", err)
		return "", "", func() {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/idle.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(idleIcon) // clients may disconnect while the icon response is written
	})
	mux.HandleFunc("/alert.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(alertIcon) // clients may disconnect while the icon response is written
	})
	// Run owns the listener and closes it when the tray exits; Serve then returns.
	go func() {
		if err := http.Serve(l, mux); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("tray: icon server stopped", "error", err)
		}
	}()
	base := fmt.Sprintf("http://%s", l.Addr().String())
	return base + "/idle.png", base + "/alert.png", func() {
		if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Warn("tray: icon server close failed", "error", err)
		}
	}
}

func setIdleState(app *menuet.Application, imageURL string) {
	app.SetMenuState(&menuet.MenuState{Image: imageURL})
}

func setAlertState(app *menuet.Application, imageURL string) {
	app.SetMenuState(&menuet.MenuState{Title: "!", Image: imageURL})
}

func pollHealth(ctx context.Context, st *State, onChange func()) {
	var last Snapshot
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Immediate first poll after short delay for api.json.
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()

	poll := func() {
		snap := fetchHealth()
		st.Set(snap)
		if snap != last {
			last = snap
			if onChange != nil {
				onChange()
			}
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		poll()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

func fetchHealth() Snapshot {
	data, err := os.ReadFile(home.APIJSONPath())
	if err != nil {
		return Snapshot{Healthy: false, Message: "api.json missing"}
	}
	var doc struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.URL == "" {
		return Snapshot{Healthy: false, Message: "api.json invalid"}
	}
	req, err := http.NewRequest(http.MethodGet, doc.URL+"/v1/system/health", nil)
	if err != nil {
		return Snapshot{Healthy: false, Message: err.Error()}
	}
	if doc.Token != "" {
		req.Header.Set("Authorization", "Bearer "+doc.Token)
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{Healthy: false, Message: "daemon unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{Healthy: false, Message: fmt.Sprintf("health HTTP %d", resp.StatusCode)}
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Snapshot{Healthy: false, Message: "bad health JSON"}
	}
	snap := Snapshot{Healthy: true, Message: "engine ok"}
	if v, ok := body["version"].(string); ok {
		snap.Version = v
	}
	if ds, ok := body["drain_state"].(map[string]any); ok {
		if d, ok := ds["draining"].(bool); ok {
			snap.Draining = d
		}
		switch n := ds["running_count"].(type) {
		case float64:
			snap.RunningCount = int(n)
		case int:
			snap.RunningCount = n
		}
	}
	switch n := body["waiting_count"].(type) {
	case float64:
		snap.WaitingCount = int(n)
	case int:
		snap.WaitingCount = n
	}
	return snap
}
