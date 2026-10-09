// Package platform owns OS service registration for trebid (LaunchAgent today).
package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/trebi-ai/trebi/internal/home"
)

// LaunchAgent labels and plist names.
const (
	DaemonLabel     = "ai.trebi.daemon"
	DaemonPlistName = "ai.trebi.daemon.plist"
	TrayLabel       = "ai.trebi.tray"
	TrayPlistName   = "ai.trebi.tray.plist"
)

// InstallOpts controls service install.
type InstallOpts struct {
	// NoTray installs only the daemon: launchd starts it at login and keeps
	// it alive. The default installs the tray too; launchd starts the tray at
	// login, and the tray starts and supervises the daemon.
	NoTray bool
	// Bin overrides binary resolution (absolute path preferred).
	Bin string
}

// PlistDef describes one LaunchAgent.
type PlistDef struct {
	Label     string
	FileName  string
	Args      []string // args after Program (absolute binary is separate)
	RunAtLoad bool
	KeepAlive bool // true = always; false = only on non-zero exit (implies RunAtLoad)
	NoRestart bool // no KeepAlive key; the tray restarts the job
	GUIOnly   bool // load only in a login session (Aqua)
	BundleID  string
	LogPath   string
}

// Service owns the LaunchAgents directory used by service operations.
type Service struct {
	plistDir string
}

// NewService constructs a service. An empty directory uses the user's default
// ~/Library/LaunchAgents location.
func NewService(plistDir string) *Service {
	return &Service{plistDir: plistDir}
}

// DaemonAgent returns the trebid agent definition. Under the tray, launchd
// does not start or restart it; the tray does. Headless, launchd does both.
// Bind address comes from config api.host/api.port (default 127.0.0.1:0).
func DaemonAgent(withTray bool) PlistDef {
	def := PlistDef{
		Label:    DaemonLabel,
		FileName: DaemonPlistName,
		Args:     []string{"service", "daemon"},
		LogPath:  home.DaemonLogPath(),
	}
	if withTray {
		def.NoRestart = true
	} else {
		def.RunAtLoad = true
		def.KeepAlive = true
	}
	return def
}

// TrayAgent returns the menu-bar tray agent: started at login, restarted
// when it fails. Quit exits 0 and stays down until the next login.
func TrayAgent() PlistDef {
	return PlistDef{
		Label:     TrayLabel,
		FileName:  TrayPlistName,
		Args:      []string{"service", "tray"},
		RunAtLoad: true,
		GUIOnly:   true,
		LogPath:   filepath.Join(home.LogsDir(), "tray.log"),
	}
}

// ResolveDaemonBinary returns an absolute path to the trebi/trebid binary.
// Order: opts override, TREBI_BIN, sibling trebid next to this process,
// this process if named trebi/trebid, then PATH (trebid, trebi).
func ResolveDaemonBinary(override string) (string, error) {
	if override != "" {
		return absExisting(override)
	}
	if env := os.Getenv("TREBI_BIN"); env != "" {
		return absExisting(env)
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		for _, name := range []string{"trebid", "trebid.exe"} {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return absExisting(p)
			}
		}
		base := strings.ToLower(filepath.Base(exe))
		switch base {
		case "trebi", "trebi.exe", "trebid", "trebid.exe":
			return absExisting(exe)
		}
	}
	for _, name := range []string{"trebid", "trebi"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		return absExisting(p)
	}
	return "", fmt.Errorf("trebi/trebid binary not found: set TREBI_BIN, use --bin, or put trebi on PATH")
}

// ProgramArguments builds launchd ProgramArguments for bin.
// trebid (basename) is invoked with no subcommand; trebi runs service daemon.
func ProgramArguments(bin string, def PlistDef) []string {
	if isTrebidBinary(bin) {
		// Future dedicated entrypoint: run as daemon with optional flags.
		args := []string{bin}
		for _, a := range def.Args {
			if a == "service" || a == "daemon" {
				continue
			}
			args = append(args, a)
		}
		return args
	}
	out := make([]string, 0, 1+len(def.Args))
	out = append(out, bin)
	out = append(out, def.Args...)
	return out
}

// Render produces LaunchAgent XML. bin must be absolute.
func (d PlistDef) Render(bin, pathEnv, homeDir string) string {
	progArgs := ProgramArguments(bin, d)
	var argLines []string
	for _, a := range progArgs {
		argLines = append(argLines, fmt.Sprintf("        <string>%s</string>", xmlEscape(a)))
	}
	if pathEnv == "" {
		pathEnv = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	if homeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			homeDir = h
		}
	}
	var extra []string
	extra = append(extra, "    <key>RunAtLoad</key>\n    "+plistBool(d.RunAtLoad))
	switch {
	case d.NoRestart:
	case d.KeepAlive:
		extra = append(extra, "    <key>KeepAlive</key>\n    <true/>")
	default:
		extra = append(extra, "    <key>KeepAlive</key>\n    <dict>\n        <key>SuccessfulExit</key>\n        <false/>\n    </dict>")
	}
	if d.GUIOnly {
		extra = append(extra, "    <key>LimitLoadToSessionType</key>\n    <string>Aqua</string>")
	}
	if d.BundleID != "" {
		// macOS shows the app name and icon for the job in Login Items.
		extra = append(extra, fmt.Sprintf("    <key>AssociatedBundleIdentifiers</key>\n    <array>\n        <string>%s</string>\n    </array>", xmlEscape(d.BundleID)))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>HOME</key>
        <string>%s</string>
    </dict>
%s
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, d.Label, strings.Join(argLines, "\n"), xmlEscape(pathEnv), xmlEscape(homeDir), strings.Join(extra, "\n"), xmlEscape(d.LogPath), xmlEscape(d.LogPath))
}

// Install writes the LaunchAgent plists with an absolute Program path.
func (s *Service) Install(opts InstallOpts) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service install is only supported on macOS (got %s)", runtime.GOOS)
	}
	bin, err := ResolveDaemonBinary(opts.Bin)
	if err != nil {
		return err
	}
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.plistDirPath(), 0o755); err != nil {
		return err
	}
	defs := []PlistDef{DaemonAgent(!opts.NoTray)}
	if !opts.NoTray {
		defs = append(defs, TrayAgent())
	} else if err := os.Remove(s.plistPath(TrayPlistName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	bundleID := ""
	if app := AppBundle(bin); app != "" {
		bundleID = bundleIdentifier(app)
		s.registerApp(app)
	}
	pathEnv := os.Getenv("PATH")
	homeDir := ""
	if h, err := os.UserHomeDir(); err == nil {
		homeDir = h
	}
	for _, def := range defs {
		def.BundleID = bundleID
		path := s.plistPath(def.FileName)
		if err := os.WriteFile(path, []byte(def.Render(bin, pathEnv, homeDir)), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Printf("Installed plist: %s\n", path)
		fmt.Printf("ProgramArguments: %s\n", strings.Join(ProgramArguments(bin, def), " "))
	}
	return nil
}

// AppBundle returns the .app directory that holds bin, or "".
func AppBundle(bin string) string {
	i := strings.Index(bin, ".app/Contents/MacOS/")
	if i < 0 {
		return ""
	}
	return bin[:i+len(".app")]
}

// Installed reports whether the daemon agent is installed.
func (s *Service) Installed() bool {
	_, err := os.Stat(s.plistPath(DaemonPlistName))
	return err == nil
}

// Uninstall stops and removes the LaunchAgent plists.
func (s *Service) Uninstall() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service uninstall is only supported on macOS (got %s)", runtime.GOOS)
	}
	s.bootout(TrayLabel)
	s.bootout(DaemonLabel)
	for _, name := range []string{TrayPlistName, DaemonPlistName} {
		if err := os.Remove(s.plistPath(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	fmt.Println("Uninstalled trebi service plists.")
	return nil
}

// Start starts the daemon and, when installed, the tray.
func (s *Service) Start() error {
	if err := s.StartDaemon(); err != nil {
		return err
	}
	return s.StartTray()
}

// Stop stops the daemon. The tray stays and shows the service as stopped.
func (s *Service) Stop() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service stop is only supported on macOS (got %s)", runtime.GOOS)
	}
	s.bootout(DaemonLabel)
	return nil
}

// Restart unloads both agents and starts them again, so that changed plists
// and binaries take effect.
func (s *Service) Restart() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service restart is only supported on macOS (got %s)", runtime.GOOS)
	}
	s.bootout(TrayLabel)
	s.bootout(DaemonLabel)
	return s.Start()
}

// StartDaemon loads the daemon agent and starts it.
func (s *Service) StartDaemon() error {
	if err := s.load(DaemonPlistName); err != nil {
		return err
	}
	return s.kickstart(DaemonLabel, false)
}

// RestartDaemon stops the running daemon and starts it again.
func (s *Service) RestartDaemon() error {
	if err := s.load(DaemonPlistName); err != nil {
		return err
	}
	return s.kickstart(DaemonLabel, true)
}

// StartTray starts the tray when its agent is installed. It is a no-op when
// the tray already runs.
func (s *Service) StartTray() error {
	if _, err := os.Stat(s.plistPath(TrayPlistName)); err != nil {
		return nil
	}
	if err := s.load(TrayPlistName); err != nil {
		return err
	}
	return s.kickstart(TrayLabel, false)
}

// DaemonState reports whether the daemon agent is loaded and whether its
// process runs. A stopped service is not loaded.
func (s *Service) DaemonState() (loaded, running bool) {
	if s.plistDir != "" {
		return false, false
	}
	out, err := exec.Command("launchctl", "list", DaemonLabel).Output()
	if err != nil {
		return false, false
	}
	return true, strings.Contains(string(out), `"PID" =`)
}

// Status reports launchd label state, one line per installed agent.
func (s *Service) Status() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("service status (OS) is only supported on macOS (got %s)", runtime.GOOS)
	}
	out, err := exec.Command("launchctl", "list").CombinedOutput()
	if err != nil {
		return "", err
	}
	_, daemonErr := os.Stat(s.plistPath(DaemonPlistName))
	lines := []string{FormatStatus(string(out), DaemonLabel, daemonErr == nil)}
	if _, err := os.Stat(s.plistPath(TrayPlistName)); err == nil {
		lines = append(lines, FormatStatus(string(out), TrayLabel, true))
	}
	return strings.Join(lines, "\n"), nil
}

// FormatStatus builds the status line of label from launchctl list output and
// whether its plist file is present. Exported for pure unit tests.
func FormatStatus(launchctlList, label string, plistInstalled bool) string {
	for _, line := range strings.Split(launchctlList, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[2] == label {
			if fields[0] == "-" {
				return fmt.Sprintf("%s: Not running (loaded, last exit %s)", label, fields[1])
			}
			return fmt.Sprintf("%s: Running (pid %s)", label, fields[0])
		}
	}
	if plistInstalled {
		return fmt.Sprintf("%s: Not running (plist installed)", label)
	}
	return fmt.Sprintf("%s: Not installed", label)
}

func plistBool(b bool) string {
	if b {
		return "<true/>"
	}
	return "<false/>"
}

func bundleIdentifier(app string) string {
	out, err := exec.Command("plutil", "-extract", "CFBundleIdentifier", "raw", filepath.Join(app, "Contents", "Info.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// registerApp tells Launch Services about the bundle, so that Login Items
// finds the name and icon of AssociatedBundleIdentifiers.
func (s *Service) registerApp(app string) {
	if s.plistDir != "" {
		return
	}
	const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
	_ = exec.Command(lsregister, "-f", app).Run() // best effort; only the Login Items label depends on it
}

// load bootstraps the agent. An agent that is already loaded is not an error.
func (s *Service) load(name string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service start is only supported on macOS (got %s)", runtime.GOOS)
	}
	path := s.plistPath(name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("plist not found (%s); run: trebi service install", path)
	}
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	// An injected directory is used for isolated installs; never touch launchctl there.
	if s.plistDir != "" {
		return nil
	}
	out, err := exec.Command("launchctl", "bootstrap", guiDomain(), path).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already bootstrapped") && !strings.Contains(string(out), "Input/output error") {
		return fmt.Errorf("launchctl bootstrap: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (s *Service) kickstart(label string, kill bool) error {
	if s.plistDir != "" {
		return nil
	}
	args := []string{"kickstart"}
	if kill {
		args = append(args, "-k")
	}
	args = append(args, guiDomain()+"/"+label)
	if out, err := exec.Command("launchctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %s: %w", label, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// bootout unloads the agent. A label that is not loaded is already stopped.
func (s *Service) bootout(label string) {
	// An injected directory is used for isolated operations; never bootout the live label.
	if s.plistDir != "" {
		return
	}
	_ = exec.Command("launchctl", "bootout", guiDomain()+"/"+label).Run()
}

func guiDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func absExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("daemon binary %s: %w", abs, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("daemon binary is a directory: %s", abs)
	}
	return abs, nil
}

// isTrebidBinary reports whether bin's final path segment is trebid[.exe].
// Handles Windows separators even when this code runs on Unix (tests / docs).
func isTrebidBinary(bin string) bool {
	base := filepath.Base(bin)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	switch strings.ToLower(base) {
	case "trebid", "trebid.exe":
		return true
	default:
		return false
	}
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func (s *Service) plistDirPath() string {
	if s.plistDir != "" {
		return s.plistDir
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("", "Library", "LaunchAgents")
	}
	return filepath.Join(h, "Library", "LaunchAgents")
}

func (s *Service) plistPath(name string) string {
	return filepath.Join(s.plistDirPath(), name)
}
