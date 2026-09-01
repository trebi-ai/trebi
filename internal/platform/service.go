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

// LaunchAgent label and plist name (frontend.md: ai.trebi.daemon).
const (
	DaemonLabel     = "ai.trebi.daemon"
	DaemonPlistName = "ai.trebi.daemon.plist"
)

// InstallOpts controls service install.
type InstallOpts struct {
	// NoTray installs a headless agent (KeepAlive always). Default is tray+daemon
	// in one process: icon means alive; Quit exits 0 and stays down (OnFailure).
	NoTray bool
	// Bin overrides binary resolution (absolute path preferred).
	Bin string
}

// PlistDef describes one LaunchAgent.
type PlistDef struct {
	Label     string
	FileName  string
	Args      []string // args after Program (absolute binary is separate)
	KeepAlive bool     // true = always; false = only on non-zero exit
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

// DaemonAgent returns the OS-supervised trebid agent definition.
// With tray (default): service daemon --listen 127.0.0.1:0, KeepAlive on failure.
// Headless: adds --no-tray, KeepAlive always.
func DaemonAgent(noTray bool) PlistDef {
	args := []string{"service", "daemon", "--listen", "127.0.0.1:0"}
	keepAlive := false // OnFailure: tray Quit (exit 0) stays quit
	if noTray {
		args = append(args, "--no-tray")
		keepAlive = true
	}
	return PlistDef{
		Label:     DaemonLabel,
		FileName:  DaemonPlistName,
		Args:      args,
		KeepAlive: keepAlive,
		LogPath:   home.DaemonLogPath(),
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
	keepAlive := "    <key>KeepAlive</key>\n    <true/>"
	if !d.KeepAlive {
		keepAlive = "    <key>KeepAlive</key>\n    <dict>\n        <key>SuccessfulExit</key>\n        <false/>\n    </dict>"
	}
	if pathEnv == "" {
		pathEnv = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	if homeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			homeDir = h
		}
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
    <key>RunAtLoad</key>
    <true/>
%s
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, d.Label, strings.Join(argLines, "\n"), xmlEscape(pathEnv), xmlEscape(homeDir), keepAlive, xmlEscape(d.LogPath), xmlEscape(d.LogPath))
}

// Install writes the LaunchAgent plist with an absolute Program path.
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
	def := DaemonAgent(opts.NoTray)
	pathEnv := os.Getenv("PATH")
	homeDir := ""
	if h, err := os.UserHomeDir(); err == nil {
		homeDir = h
	}
	content := def.Render(bin, pathEnv, homeDir)
	path := s.plistPath(def.FileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Printf("Installed plist: %s\n", path)
	fmt.Printf("ProgramArguments: %s\n", strings.Join(ProgramArguments(bin, def), " "))
	return nil
}

// Uninstall stops and removes the LaunchAgent plist.
func (s *Service) Uninstall() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service uninstall is only supported on macOS (got %s)", runtime.GOOS)
	}
	if err := s.Stop(); err != nil {
		return err
	}
	path := s.plistPath(DaemonPlistName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Uninstalled trebi service plist.")
	return nil
}

// Start loads the LaunchAgent via launchctl bootstrap.
func (s *Service) Start() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service start is only supported on macOS (got %s)", runtime.GOOS)
	}
	path := s.plistPath(DaemonPlistName)
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
	target := fmt.Sprintf("gui/%d", os.Getuid())
	out, err := exec.Command("launchctl", "bootstrap", target, path).CombinedOutput()
	if err != nil {
		// Already loaded is often reported as error; try kickstart.
		if strings.Contains(string(out), "already bootstrapped") || strings.Contains(string(out), "Input/output error") {
			if kickErr := exec.Command("launchctl", "kickstart", "-k", target+"/"+DaemonLabel).Run(); kickErr != nil {
				return fmt.Errorf("launchctl kickstart: %w", kickErr)
			}
			return nil
		}
		return fmt.Errorf("launchctl bootstrap: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Stop unloads the LaunchAgent via launchctl bootout.
func (s *Service) Stop() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service stop is only supported on macOS (got %s)", runtime.GOOS)
	}
	// An injected directory is used for isolated operations; never bootout the live label.
	if s.plistDir != "" {
		return nil
	}
	target := fmt.Sprintf("gui/%d", os.Getuid())
	if err := exec.Command("launchctl", "bootout", target+"/"+DaemonLabel).Run(); err != nil {
		// bootout is idempotent; a missing label is already stopped.
		return nil
	}
	return nil
}

// Restart is stop then start.
func (s *Service) Restart() error {
	if err := s.Stop(); err != nil {
		return err
	}
	return s.Start()
}

// Status reports launchd label state (one line).
func (s *Service) Status() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("service status (OS) is only supported on macOS (got %s)", runtime.GOOS)
	}
	out, err := exec.Command("launchctl", "list").CombinedOutput()
	if err != nil {
		return "", err
	}
	_, plistErr := os.Stat(s.plistPath(DaemonPlistName))
	return FormatStatus(string(out), plistErr == nil), nil
}

// Install uses the default LaunchAgents directory.
func Install(opts InstallOpts) error { return NewService("").Install(opts) }

// Uninstall uses the default LaunchAgents directory.
func Uninstall() error { return NewService("").Uninstall() }

// Start uses the default LaunchAgents directory.
func Start() error { return NewService("").Start() }

// Stop uses the default LaunchAgents directory.
func Stop() error { return NewService("").Stop() }

// Restart uses the default LaunchAgents directory.
func Restart() error { return NewService("").Restart() }

// Status uses the default LaunchAgents directory.
func Status() (string, error) { return NewService("").Status() }

// FormatStatus builds the status line from launchctl list output and whether
// the plist file is present. Exported for pure unit tests.
func FormatStatus(launchctlList string, plistInstalled bool) string {
	for _, line := range strings.Split(launchctlList, "\n") {
		if strings.Contains(line, DaemonLabel) {
			return fmt.Sprintf("%s: Running (%s)", DaemonLabel, strings.TrimSpace(line))
		}
	}
	if plistInstalled {
		return fmt.Sprintf("%s: Not running (plist installed)", DaemonLabel)
	}
	return fmt.Sprintf("%s: Not installed", DaemonLabel)
}

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
