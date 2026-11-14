package tray

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/update"
)

// Controller starts and stops the service. The tray owns no launchd logic.
type Controller interface {
	StartDaemon() error
	RestartDaemon() error
	Stop() error
	DaemonState() (loaded, running bool)
}

// Tray is the menu-bar app. It starts the service, restarts it when it stops
// on its own, and leaves it down when the user stops it.
type Tray struct {
	ctl   Controller
	shell Shell
	log   *slog.Logger
	ctx   context.Context // process lifetime; set by Run
	up    *update.Updater // nil: this copy has no updates

	mu        sync.Mutex
	snap      Snapshot
	items     []Item
	lastStart time.Time
	feedBase  string
	feedStop  context.CancelFunc
	// reported is true after the tray told this daemon run the permission.
	reported bool
}

// New builds a tray over ctl. shell opens the app window.
func New(ctl Controller, shell Shell) *Tray {
	return &Tray{ctl: ctl, shell: shell, log: slog.Default().With("source", "tray")}
}

// SetUpdater turns on "Check for updates…" and the automatic check.
func (t *Tray) SetUpdater(u *update.Updater) { t.up = u }

// Status is the service state that the tray shows.
type Status int

const (
	StatusStopped Status = iota
	StatusStarting
	StatusRunning
	StatusNotResponding
)

// Snapshot is what the tray renders.
type Snapshot struct {
	Status         Status
	URL            string
	Version        string
	EditionVersion string
	UIVersion      string
	Draining       bool
	RunningCount   int
	WaitingCount   int
	HasShell       bool
}

// Action is what a menu item does when clicked. ActionNone is a text row.
type Action int

const (
	ActionNone Action = iota
	ActionOpenApp
	ActionOpenBrowser
	ActionStart
	ActionRestart
	ActionStop
	ActionOpenLog
	ActionShowFolder
	ActionCheckUpdates
	ActionAbout
	ActionQuit
)

// Item is one menu row.
type Item struct {
	Text      string
	Action    Action
	Separator bool
}

// Menu returns the menu rows for the snapshot.
func (s Snapshot) Menu() []Item {
	items := []Item{{Text: s.headline()}}
	if s.Status == StatusRunning {
		items = append(items, Item{Text: s.activity()})
	}
	// Without the shell, "Open Trebi" opens the browser.
	open := Item{Text: "Open Trebi"}
	browser := Item{Text: "Open Trebi in Browser"}
	if s.Status == StatusRunning {
		open.Action = ActionOpenBrowser
		browser.Action = ActionOpenBrowser
		if s.HasShell {
			open.Action = ActionOpenApp
		}
	}
	items = append(items, Item{Separator: true}, open)
	if s.HasShell {
		items = append(items, browser)
	}
	items = append(items, Item{Separator: true})
	if s.Status == StatusStopped {
		items = append(items, Item{Text: "Start Trebi", Action: ActionStart})
	} else {
		items = append(items,
			Item{Text: "Restart Trebi", Action: ActionRestart},
			Item{Text: "Stop Trebi", Action: ActionStop})
	}
	return append(items,
		Item{Separator: true},
		Item{Text: "Troubleshooting Info…", Action: ActionOpenLog},
		Item{Text: "Show Trebi Folder", Action: ActionShowFolder},
		Item{Separator: true},
		Item{Text: "Check for updates…", Action: ActionCheckUpdates},
		Item{Text: "About Trebi", Action: ActionAbout},
		Item{Text: "Quit Trebi", Action: ActionQuit})
}

// Title is the text next to the icon: the count of items that wait for the user.
func (s Snapshot) Title() string {
	if s.Status == StatusRunning && s.WaitingCount > 0 {
		return strconv.Itoa(s.WaitingCount)
	}
	return ""
}

func (s Snapshot) headline() string {
	switch s.Status {
	case StatusRunning:
		if s.Draining {
			return "Trebi is running · new runs paused"
		}
		return "Trebi is running"
	case StatusStarting:
		return "Trebi is starting…"
	case StatusNotResponding:
		return "Trebi is not responding"
	default:
		return "Trebi is stopped"
	}
}

func (s Snapshot) activity() string {
	line := "No runs active"
	switch s.RunningCount {
	case 0:
	case 1:
		line = "1 run active"
	default:
		line = strconv.Itoa(s.RunningCount) + " runs active"
	}
	if s.WaitingCount > 0 {
		line += " · " + strconv.Itoa(s.WaitingCount) + " waiting for you"
	}
	return line
}

func (s Snapshot) about() string {
	text := "Version " + orUnknown(s.Version)
	if s.EditionVersion != "" {
		text += "\nEdition " + s.EditionVersion
	}
	if s.UIVersion != "" {
		text += "\nUI " + s.UIVersion
	}
	return text
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
