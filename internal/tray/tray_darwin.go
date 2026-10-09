//go:build darwin && cgo

package tray

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include <stdint.h>
#include <stdlib.h>
#include "menubar_darwin.h"
*/
import "C"

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"runtime/cgo"
	"time"
	"unsafe"

	"github.com/trebi-ai/trebi/internal/home"
)

//go:embed assets/running.png
var runningIcon []byte

//go:embed assets/stopped.png
var stoppedIcon []byte

// Available is true on macOS with cgo.
const Available = true

const (
	pollEvery = 2 * time.Second
	// A daemon that has run for less than this is starting, not stuck.
	startGrace = 30 * time.Second
	// The least time between two automatic starts of the daemon.
	restartBackoff = 10 * time.Second
)

// Run shows the tray and blocks on the main thread until the user quits.
// app.Main pins the main goroutine to the main OS thread.
func (t *Tray) Run(ctx context.Context) error {
	t.ctx = ctx
	h := cgo.NewHandle(t)
	defer h.Delete()
	C.trayRun(C.uintptr_t(h))
	return nil
}

//export trayReady
func trayReady(h C.uintptr_t) {
	t := cgo.Handle(h).Value().(*Tray)
	go t.supervise()
}

//export trayClicked
func trayClicked(h C.uintptr_t, id C.int) {
	t := cgo.Handle(h).Value().(*Tray)
	t.mu.Lock()
	var action Action
	if i := int(id) - 1; i >= 0 && i < len(t.items) {
		action = t.items[i].Action
	}
	t.mu.Unlock()
	// The click arrives on the main thread; dialogs block, so leave it.
	go t.do(action)
}

func (t *Tray) supervise() {
	t.start()
	t.refresh(true)
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.refresh(false)
		}
	}
}

func (t *Tray) start() {
	t.mu.Lock()
	t.lastStart = time.Now()
	t.mu.Unlock()
	if err := t.ctl.StartDaemon(); err != nil {
		t.log.Warn("tray: start service failed", "error", err)
	}
}

// refresh reads the service state, restarts a service that stopped on its
// own, and redraws when the state changed.
func (t *Tray) refresh(force bool) {
	loaded, running := t.ctl.DaemonState()
	t.mu.Lock()
	sinceStart := time.Since(t.lastStart)
	t.mu.Unlock()

	var snap Snapshot
	switch {
	case !loaded:
		snap.Status = StatusStopped
	case !running:
		snap.Status = StatusStarting
		if sinceStart > restartBackoff {
			t.log.Info("tray: service is not running; starting it")
			t.start()
		}
	default:
		snap = fetchHealth()
		if snap.Status != StatusRunning && sinceStart < startGrace {
			snap.Status = StatusStarting
		}
	}

	t.mu.Lock()
	changed := force || snap != t.snap
	t.snap = snap
	if changed {
		t.items = snap.Menu()
	}
	items := t.items
	t.mu.Unlock()
	if changed {
		t.draw(snap, items)
	}
}

func (t *Tray) draw(snap Snapshot, items []Item) {
	icon := stoppedIcon
	if snap.Status == StatusRunning {
		icon = runningIcon
	}
	title := C.CString(snap.Title())
	defer C.free(unsafe.Pointer(title))
	C.traySetIcon(unsafe.Pointer(&icon[0]), C.int(len(icon)), title)

	type row struct {
		Text      string `json:"text,omitempty"`
		ID        int    `json:"id,omitempty"`
		Separator bool   `json:"separator,omitempty"`
	}
	rows := make([]row, len(items))
	for i, it := range items {
		rows[i] = row{Text: it.Text, Separator: it.Separator}
		if it.Action != ActionNone {
			rows[i].ID = i + 1
		}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.log.Warn("tray: menu encode failed", "error", err)
		return
	}
	menu := C.CString(string(data))
	defer C.free(unsafe.Pointer(menu))
	C.traySetMenu(menu)
}

func (t *Tray) do(action Action) {
	t.mu.Lock()
	snap := t.snap
	t.mu.Unlock()
	switch action {
	case ActionOpen:
		t.open(snap.URL)
	case ActionStart:
		t.start()
	case ActionRestart:
		t.mu.Lock()
		t.lastStart = time.Now()
		t.mu.Unlock()
		if err := t.ctl.RestartDaemon(); err != nil {
			t.log.Warn("tray: restart service failed", "error", err)
		}
	case ActionStop:
		if err := t.ctl.Stop(); err != nil {
			t.log.Warn("tray: stop service failed", "error", err)
		}
	case ActionOpenLog:
		if err := exec.Command("open", "-a", "Console", home.DaemonLogPath()).Run(); err != nil {
			t.open(home.DaemonLogPath())
		}
	case ActionShowFolder:
		t.open(home.Base())
	case ActionAbout:
		alert("Trebi", snap.about(), "OK", "")
	case ActionQuit:
		if !alert("Quit Trebi?", "This stops the Trebi service. Scheduled jobs do not run until you open Trebi again.", "Quit", "Cancel") {
			return
		}
		if err := t.ctl.Stop(); err != nil {
			t.log.Warn("tray: stop service failed", "error", err)
		}
		// Exit 0, so launchd does not start the tray again before the next login.
		C.trayTerminate()
		return
	default:
		return
	}
	t.refresh(true)
}

func (t *Tray) open(target string) {
	if target == "" {
		return
	}
	if err := exec.Command("open", target).Run(); err != nil {
		t.log.Warn("tray: open failed", "target", target, "error", err)
	}
}

func alert(message, info, ok, cancel string) bool {
	cs := []*C.char{C.CString(message), C.CString(info), C.CString(ok), C.CString(cancel)}
	defer func() {
		for _, c := range cs {
			C.free(unsafe.Pointer(c))
		}
	}()
	return C.trayAlert(cs[0], cs[1], cs[2], cs[3]) == 1
}

func fetchHealth() Snapshot {
	not := Snapshot{Status: StatusNotResponding}
	data, err := os.ReadFile(home.APIJSONPath())
	if err != nil {
		return not
	}
	var doc struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.URL == "" {
		return not
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(doc.URL + "/v1/system/health")
	if err != nil {
		return not
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return not
	}
	var body struct {
		Version        string `json:"version"`
		EditionVersion string `json:"edition_version"`
		UIVersion      string `json:"ui_version"`
		WaitingCount   int    `json:"waiting_count"`
		DrainState     struct {
			Draining     bool `json:"draining"`
			RunningCount int  `json:"running_count"`
		} `json:"drain_state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return not
	}
	return Snapshot{
		Status:         StatusRunning,
		URL:            doc.URL,
		Version:        body.Version,
		EditionVersion: body.EditionVersion,
		UIVersion:      body.UIVersion,
		Draining:       body.DrainState.Draining,
		RunningCount:   body.DrainState.RunningCount,
		WaitingCount:   body.WaitingCount,
	}
}
