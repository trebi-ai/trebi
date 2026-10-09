package tray

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/platform"
)

const (
	// ShellScheme is the private URL scheme of the window helper.
	ShellScheme = "ai.trebi.shell"
	// DefaultURL is the daemon URL before the first api.json exists.
	DefaultURL = "http://127.0.0.1:7433"
)

// Shell starts the window helper inside the app bundle.
type Shell struct {
	app string // …/Trebi.app/Contents/Helpers/TrebiShell.app; "" when absent
}

// NewShell finds the helper in the bundle that holds exe.
func NewShell(exe string) Shell {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	bundle := platform.AppBundle(exe)
	if bundle == "" {
		return Shell{}
	}
	app := filepath.Join(bundle, "Contents", "Helpers", "TrebiShell.app")
	if st, err := os.Stat(app); err != nil || !st.IsDir() {
		return Shell{}
	}
	return Shell{app: app}
}

func (s Shell) Available() bool { return s.app != "" }

// OpenURL returns the private URL that tells the shell what to show.
// path must start with "/".
func (s Shell) OpenURL(base, path string) string {
	return ShellScheme + "://open?" + url.Values{"base": {base}, "path": {path}}.Encode()
}

// Open starts the shell, or sends the URL to the running shell.
func (s Shell) Open(base, path string) error {
	if s.app == "" {
		return fmt.Errorf("no window shell in this build")
	}
	if out, err := exec.Command("open", "-a", s.app, s.OpenURL(base, path)).CombinedOutput(); err != nil {
		return fmt.Errorf("open shell: %s: %w", out, err)
	}
	return nil
}

// DaemonURL is the URL in api.json, or DefaultURL before the first start.
func DaemonURL() string {
	data, err := os.ReadFile(home.APIJSONPath())
	if err != nil {
		return DefaultURL
	}
	var doc struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.URL == "" {
		return DefaultURL
	}
	return doc.URL
}

// RouteFromLink maps trebi://run/abc?tab=session to /run/abc?tab=session.
// It returns an error for any other scheme. The result is only a UI path.
func RouteFromLink(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("parse link: %w", err)
	}
	if u.Scheme != "trebi" {
		return "", fmt.Errorf("not a trebi:// link: %q", link)
	}
	p := u.Host + u.Path
	if u.Opaque != "" {
		p = u.Opaque
	}
	route := path.Clean("/" + p)
	if u.RawQuery != "" {
		route += "?" + u.RawQuery
	}
	return route, nil
}
