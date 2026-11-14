//go:build darwin && cgo

package tray

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework UserNotifications
#include <stdint.h>
#include <stdlib.h>
#include "menubar_darwin.h"
*/
import "C"

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"runtime/cgo"
	"strings"
	"time"
	"unsafe"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/update"
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
	// The first automatic update check waits, so it does not compete with the start.
	updateFirst = 5 * time.Minute
	updateEvery = 6 * time.Hour
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
	go t.autoUpdate()
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

// The user opened Trebi.app again while the tray runs.
//
//export trayReopen
func trayReopen(h C.uintptr_t) {
	t := cgo.Handle(h).Value().(*Tray)
	go t.openApp("/")
}

//export trayOpenLink
func trayOpenLink(h C.uintptr_t, link *C.char) {
	t := cgo.Handle(h).Value().(*Tray)
	route, err := RouteFromLink(C.GoString(link))
	if err != nil {
		t.log.Warn("tray: deep link rejected", "err", err)
		return
	}
	go t.openApp(route)
}

//export trayNotificationClicked
func trayNotificationClicked(h C.uintptr_t, route *C.char) {
	t := cgo.Handle(h).Value().(*Tray)
	r := C.GoString(route)
	if !strings.HasPrefix(r, "/") {
		r = "/"
	}
	go t.openApp(r)
}

// The user answered the permission prompt.
//
//export trayNotificationStatus
func trayNotificationStatus(h C.uintptr_t, status C.int) {
	t := cgo.Handle(h).Value().(*Tray)
	t.mu.Lock()
	base := t.snap.URL
	t.mu.Unlock()
	if base == "" {
		base = DaemonURL()
	}
	go t.reportNotifications(base, int(status))
}

// reportNotifications sends the permission to the daemon. code is the value
// of trayNotificationAuthorization.
func (t *Tray) reportNotifications(base string, code int) {
	status := NotificationsNotDetermined
	switch code {
	case 1:
		status = NotificationsDenied
	case 2:
		status = NotificationsAllowed
	}
	ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	defer cancel()
	if err := ReportNotifications(ctx, &http.Client{}, base, status); err != nil {
		t.log.Warn("tray: report notifications failed", "err", err)
	}
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
	snap.HasShell = t.shell.Available()
	if snap.Status == StatusRunning {
		t.follow(snap.URL)
	}

	t.mu.Lock()
	report := snap.Status == StatusRunning && !t.reported
	t.reported = snap.Status == StatusRunning
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
	if report {
		go t.reportNotifications(snap.URL, int(C.trayNotificationAuthorization()))
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
	case ActionOpenApp:
		t.openApp("/")
	case ActionOpenBrowser:
		t.open(snap.URL + "/")
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
	case ActionCheckUpdates:
		t.checkUpdates(true)
	case ActionAbout:
		alert("Trebi", snap.about(), "OK", "")
	case ActionQuit:
		if !alert("Quit Trebi?", "This stops Trebi. Scheduled jobs do not run until you open Trebi again.", "Quit", "Cancel") {
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

// autoUpdate checks for a release every updateEvery.
func (t *Tray) autoUpdate() {
	timer := time.NewTimer(updateFirst)
	defer timer.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-timer.C:
		}
		t.checkUpdates(false)
		timer.Reset(updateEvery)
	}
}

// checkUpdates stages a newer release and installs it when no run is active.
// Only a user check shows a message.
func (t *Tray) checkUpdates(user bool) {
	if t.up == nil {
		if user {
			alert("Trebi", "This copy of Trebi does not update itself.", "OK", "")
		}
		return
	}
	plan, err := t.up.Prepare(t.ctx)
	if err != nil && !errors.Is(err, update.ErrBusy) {
		t.log.Warn("tray: update check failed", "error", err)
	}
	if err == nil && plan.Kind == update.PlanManual {
		t.log.Info("tray: update needs a download", "reason", plan.Reason)
	}
	if err == nil && plan.Kind == update.PlanReady {
		go func() {
			if err := t.up.ApplyWhenIdle(t.ctx, plan.Staged); err != nil {
				t.log.Warn("tray: update failed", "error", err)
			}
		}()
	}
	if !user {
		return
	}
	t.mu.Lock()
	version := t.snap.Version
	t.mu.Unlock()
	a := NewUpdateAlert(plan, err, version)
	if alert(a.Title, a.Info, a.OK, a.Cancel) && err == nil && plan.Kind == update.PlanManual {
		t.open(plan.Release.DMGURL)
	}
}

func (t *Tray) open(target string) {
	if target == "" {
		return
	}
	if err := exec.Command("open", target).Run(); err != nil {
		t.log.Warn("tray: open failed", "target", target, "error", err)
	}
}

// openApp opens the shell at path, or the browser when no shell exists.
func (t *Tray) openApp(path string) {
	t.mu.Lock()
	base := t.snap.URL
	t.mu.Unlock()
	if base == "" {
		base = DaemonURL()
	}
	if !t.shell.Available() {
		t.open(base + path)
		return
	}
	if err := t.shell.Open(base, path); err != nil {
		t.log.Warn("tray: open shell failed", "err", err)
		t.open(base + path)
	}
}

// follow starts the notification feed for base, and stops the feed of an
// earlier base. The feed stops with the tray.
func (t *Tray) follow(base string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if base == t.feedBase {
		return
	}
	if t.feedStop != nil {
		t.feedStop()
	}
	ctx, cancel := context.WithCancel(t.ctx)
	t.feedBase, t.feedStop = base, cancel
	go NewFeed(base, notify, withdraw, requestNotifications).Run(ctx)
}

func notify(n Notice) {
	cs := []*C.char{C.CString(n.ID), C.CString(n.Title), C.CString(n.Body), C.CString(n.Route)}
	defer func() {
		for _, c := range cs {
			C.free(unsafe.Pointer(c))
		}
	}()
	C.trayNotify(cs[0], cs[1], cs[2], cs[3])
}

func requestNotifications() { C.trayRequestNotifications() }

func withdraw(id string) {
	c := C.CString(id)
	defer C.free(unsafe.Pointer(c))
	C.trayWithdraw(c)
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
	base := DaemonURL()
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(base + "/v1/system/health")
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
		URL:            base,
		Version:        body.Version,
		EditionVersion: body.EditionVersion,
		UIVersion:      body.UIVersion,
		Draining:       body.DrainState.Draining,
		RunningCount:   body.DrainState.RunningCount,
		WaitingCount:   body.WaitingCount,
	}
}
