package tray

import (
	"errors"

	"github.com/trebi-ai/trebi/internal/update"
)

// UpdateAlert is the text of the alert that a user check for updates shows.
type UpdateAlert struct {
	Title, Info, OK, Cancel string
}

// NewUpdateAlert words the result of Prepare for the user. It never shows the
// raw error. version is the running version.
func NewUpdateAlert(p update.Plan, err error, version string) UpdateAlert {
	switch {
	case errors.Is(err, update.ErrBusy):
		return UpdateAlert{Title: "Trebi", Info: "An update waits for the runs to end.", OK: "OK"}
	case errors.Is(err, update.ErrCheck):
		return UpdateAlert{Title: "Trebi cannot check for updates now", Info: "Try again later.", OK: "OK"}
	case err != nil:
		return UpdateAlert{Title: "Trebi cannot download the update now", Info: "Try again later.", OK: "OK"}
	}
	switch p.Kind {
	case update.PlanManual:
		return UpdateAlert{Title: "Version " + p.Release.Version() + " is available", Info: "Download the new version.", OK: "Download", Cancel: "Later"}
	case update.PlanReady:
		return UpdateAlert{Title: "Update ready", Info: "Trebi installs version " + p.Release.Version() + " when no run is active.", OK: "OK"}
	default:
		info := "You have the newest version."
		if version != "" {
			info = "You have version " + version + "."
		}
		return UpdateAlert{Title: "Trebi is up to date", Info: info, OK: "OK"}
	}
}
