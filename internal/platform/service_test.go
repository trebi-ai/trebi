package platform

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestRenderPlist_AbsoluteProgramArguments(t *testing.T) {
	t.Parallel()
	// Under the tray, launchd neither starts nor restarts the daemon.
	def := DaemonAgent(true)
	def.LogPath = "/tmp/trebid.log"
	def.BundleID = "ai.trebi.app"
	bin := "/Users/me/Applications/Trebi.app/Contents/MacOS/trebi"
	got := def.Render(bin, "/usr/bin:/bin", "/Users/me")

	mustContain(t, got, "<string>ai.trebi.daemon</string>")
	mustContain(t, got, "<string>/Users/me/Applications/Trebi.app/Contents/MacOS/trebi</string>")
	mustContain(t, got, "<string>service</string>")
	mustContain(t, got, "<string>daemon</string>")
	mustContain(t, got, "<string>/Users/me</string>")
	mustContain(t, got, "<string>/tmp/trebid.log</string>")
	mustContain(t, got, "<key>RunAtLoad</key>\n    <false/>")
	mustContain(t, got, "<key>AssociatedBundleIdentifiers</key>\n    <array>\n        <string>ai.trebi.app</string>")
	if strings.Contains(got, "KeepAlive") {
		t.Fatalf("daemon under the tray must have no KeepAlive:\n%s", got)
	}
	if strings.Contains(got, "--listen") {
		t.Fatalf("install must not hardcode --listen; bind comes from api.host/api.port:\n%s", got)
	}

	// Absolute path must appear before subcommands.
	idxBin := strings.Index(got, bin)
	idxSvc := strings.Index(got, "<string>service</string>")
	if idxBin < 0 || idxSvc < 0 || idxBin > idxSvc {
		t.Fatalf("absolute bin must precede service args\n%s", got)
	}

	headless := DaemonAgent(false)
	gotH := headless.Render(bin, "/usr/bin:/bin", "/Users/me")
	mustContain(t, gotH, "<key>RunAtLoad</key>\n    <true/>")
	mustContain(t, gotH, "<key>KeepAlive</key>\n    <true/>")
	if strings.Contains(gotH, "AssociatedBundleIdentifiers") {
		t.Fatalf("no bundle id, no association:\n%s", gotH)
	}

	tray := TrayAgent()
	gotT := tray.Render(bin, "", "/Users/me")
	mustContain(t, gotT, "<string>ai.trebi.tray</string>")
	mustContain(t, gotT, "<string>tray</string>")
	mustContain(t, gotT, "<key>RunAtLoad</key>\n    <true/>")
	mustContain(t, gotT, "<key>SuccessfulExit</key>\n        <false/>")
	mustContain(t, gotT, "<key>LimitLoadToSessionType</key>\n    <string>Aqua</string>")
}

func TestAppBundle(t *testing.T) {
	t.Parallel()
	for bin, want := range map[string]string{
		"/Applications/Trebi.app/Contents/MacOS/trebi":                  "/Applications/Trebi.app",
		"/opt/homebrew/Cellar/trebi/1.0/Trebi.app/Contents/MacOS/trebi": "/opt/homebrew/Cellar/trebi/1.0/Trebi.app",
		"/usr/local/bin/trebi":                                          "",
	} {
		if got := AppBundle(bin); got != want {
			t.Errorf("AppBundle(%q) = %q, want %q", bin, got, want)
		}
	}
}

func TestRenderPlist_XMLEscapeAndKeepAliveOnFailure(t *testing.T) {
	t.Parallel()
	def := PlistDef{
		Label:     "ai.trebi.daemon",
		FileName:  DaemonPlistName,
		Args:      []string{"service", "daemon"},
		KeepAlive: false,
		LogPath:   "/tmp/a&b<c>.log",
	}
	got := def.Render("/opt/bin/trebi", "a&b:/bin", "/Users/a&b")
	mustContain(t, got, "<string>a&amp;b:/bin</string>")
	mustContain(t, got, "<string>/Users/a&amp;b</string>")
	mustContain(t, got, "<string>/tmp/a&amp;b&lt;c&gt;.log</string>")
	mustContain(t, got, "<key>SuccessfulExit</key>")
}

func TestRenderPlist_DefaultPathAndHome(t *testing.T) {
	t.Parallel()
	def := DaemonAgent(false)
	def.LogPath = "/tmp/t.log"
	got := def.Render("/bin/trebi", "", "")
	mustContain(t, got, "<string>/usr/bin:/bin:/usr/sbin:/sbin</string>")
	// HOME falls back to user home; still present as a key.
	mustContain(t, got, "<key>HOME</key>")
}

func TestProgramArguments_TrebiVsTrebid(t *testing.T) {
	t.Parallel()
	def := DaemonAgent(false)
	def.Args = append(def.Args, "--shadow")
	trebi := ProgramArguments("/opt/trebi/trebi", def)
	if len(trebi) < 3 || trebi[0] != "/opt/trebi/trebi" || trebi[1] != "service" || trebi[2] != "daemon" {
		t.Fatalf("trebi args: %v", trebi)
	}
	trebid := ProgramArguments("/opt/trebi/trebid", def)
	if trebid[0] != "/opt/trebi/trebid" {
		t.Fatalf("trebid bin: %v", trebid)
	}
	for _, a := range trebid {
		if a == "service" || a == "daemon" {
			t.Fatalf("trebid must not get service/daemon subcommands: %v", trebid)
		}
	}
	if !contains(trebid, "--shadow") {
		t.Fatalf("trebid should keep daemon flags: %v", trebid)
	}
	// Windows-style basename
	win := ProgramArguments(`C:\Programs\Trebi\trebid.exe`, def)
	if win[0] != `C:\Programs\Trebi\trebid.exe` {
		t.Fatalf("trebid.exe: %v", win)
	}
	for _, a := range win {
		if a == "service" {
			t.Fatalf("trebid.exe must skip service: %v", win)
		}
	}
}

func TestResolveDaemonBinary_Override(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "trebi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveDaemonBinary(bin)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(bin)
	want, _ = filepath.Abs(want)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveDaemonBinary_Missing(t *testing.T) {
	t.Parallel()
	_, err := ResolveDaemonBinary(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestResolveDaemonBinary_DirectoryRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := ResolveDaemonBinary(dir)
	if err == nil {
		t.Fatal("expected error for directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want directory error, got %v", err)
	}
}

func TestResolveDaemonBinary_TREBI_BIN(t *testing.T) {
	dir := t.TempDir()
	trebid := filepath.Join(dir, "trebid")
	if err := os.WriteFile(trebid, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TREBI_BIN", trebid)
	got, err := ResolveDaemonBinary("")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "trebid" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveDaemonBinary_OverrideBeatsEnv(t *testing.T) {
	dir := t.TempDir()
	envBin := filepath.Join(dir, "env-trebi")
	optBin := filepath.Join(dir, "opt-trebi")
	for _, p := range []string{envBin, optBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TREBI_BIN", envBin)
	got, err := ResolveDaemonBinary(optBin)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "opt-trebi" {
		t.Fatalf("override should win: got %q", got)
	}
}

func TestDaemonAgent_Defaults(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	managed := DaemonAgent(true)
	if managed.Label != DaemonLabel || managed.FileName != DaemonPlistName {
		t.Fatalf("label/file: %s %s", managed.Label, managed.FileName)
	}
	if managed.RunAtLoad || !managed.NoRestart {
		t.Fatalf("under the tray: want no RunAtLoad and no restart: %+v", managed)
	}
	headless := DaemonAgent(false)
	if !headless.RunAtLoad || !headless.KeepAlive {
		t.Fatalf("headless: want RunAtLoad and KeepAlive always: %+v", headless)
	}
	if !reflect.DeepEqual(headless.Args, []string{"service", "daemon"}) {
		t.Fatalf("headless args: %v", headless.Args)
	}
	if !strings.HasSuffix(headless.LogPath, filepath.Join("logs", "trebid.log")) {
		t.Fatalf("log path: %s", headless.LogPath)
	}
	if got := TrayAgent().LogPath; !strings.HasSuffix(got, filepath.Join("logs", "tray.log")) {
		t.Fatalf("tray log path: %s", got)
	}
}

func TestFormatStatus(t *testing.T) {
	t.Parallel()
	list := "123\t0\tai.trebi.daemon\n-\t0\tai.trebi.tray\nother"
	tests := []struct {
		label     string
		installed bool
		want      string
	}{
		{DaemonLabel, true, DaemonLabel + ": Running (pid 123)"},
		{TrayLabel, true, TrayLabel + ": Not running (loaded, last exit 0)"},
		{"ai.trebi.other", true, "ai.trebi.other: Not running (plist installed)"},
		{"ai.trebi.other", false, "ai.trebi.other: Not installed"},
	}
	for _, tt := range tests {
		if got := FormatStatus(list, tt.label, tt.installed); got != tt.want {
			t.Errorf("FormatStatus(%s) = %q, want %q", tt.label, got, tt.want)
		}
	}
}

func TestInstallWritesAbsoluteProgramPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("install is darwin-only")
	}
	agents := t.TempDir()
	svc := NewService(agents)

	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "trebi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := svc.Install(InstallOpts{Bin: bin}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(agents, DaemonPlistName)
	if _, err := os.Stat(filepath.Join(agents, TrayPlistName)); err != nil {
		t.Fatalf("default install writes the tray agent: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	abs, _ := filepath.Abs(bin)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	mustContain(t, body, "<string>"+abs+"</string>")
	mustContain(t, body, "<string>service</string>")
	mustContain(t, body, "<string>daemon</string>")
	if strings.Contains(body, "KeepAlive") {
		t.Fatal("default install: the tray restarts the daemon, not launchd")
	}
	mustContain(t, body, home.DaemonLogPath())

	// Uninstall removes the test plist only.
	if err := svc.Uninstall(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DaemonPlistName, TrayPlistName} {
		if _, err := os.Stat(filepath.Join(agents, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, err=%v", name, err)
		}
	}
}

func TestInstallMissingBin(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("install is darwin-only")
	}
	svc := NewService(t.TempDir())
	err := svc.Install(InstallOpts{Bin: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestStartMissingPlist(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("start is darwin-only")
	}
	svc := NewService(t.TempDir())
	err := svc.Start()
	if err == nil {
		t.Fatal("expected missing plist error")
	}
	if !strings.Contains(err.Error(), "plist not found") {
		t.Fatalf("got %v", err)
	}
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q\nfull:\n%s", want, got)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestZoneName(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, env, link, etc, want string }{
		{"env", "America/Sao_Paulo", "", "", "America/Sao_Paulo"},
		{"env colon", ":America/Sao_Paulo", "", "", "America/Sao_Paulo"},
		{"macOS link", "", "/var/db/timezone/zoneinfo/Europe/Berlin", "", "Europe/Berlin"},
		{"linux link", "", "/usr/share/zoneinfo/Asia/Tokyo", "", "Asia/Tokyo"},
		{"etc timezone", "", "", "Europe/Lisbon\n", "Europe/Lisbon"},
		{"bogus env", "Bogus/Zone", "", "", ""},
		{"local env", "Local", "", "", ""},
		{"utc env", "UTC", "", "", "UTC"},
		{"link without zoneinfo", "", "/etc/localtime.copy", "", ""},
		{"all empty", "", "", "", ""},
		{"path env", "/usr/share/zoneinfo/Asia/Tokyo", "", "", ""},
		{"bad env falls through", "Bogus/Zone", "/usr/share/zoneinfo/Asia/Tokyo", "", "Asia/Tokyo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := zoneName(tc.env, tc.link, tc.etc); got != tc.want {
				t.Errorf("zoneName(%q, %q, %q) = %q, want %q", tc.env, tc.link, tc.etc, got, tc.want)
			}
		})
	}
}

func TestUserPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/paths", "/usr/local/bin\n/usr/bin\n")
	write("etc/paths.d/10-a", "/opt/a/bin\n")
	write("etc/paths.d/20-b", "/opt/b/bin\n/usr/bin\n")

	got := userPath(root, "/Users/me", "/x/bin::/usr/bin")
	want := "/x/bin:/usr/bin:/usr/local/bin:/opt/a/bin:/opt/b/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/Users/me/.local/bin:/bin:/usr/sbin:/sbin"
	if got != want {
		t.Errorf("userPath = %q, want %q", got, want)
	}
	if got := userPath(t.TempDir(), "", ""); got != "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("empty input = %q", got)
	}
}

func TestServiceInstallPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("install is darwin-only")
	}
	agents := t.TempDir()
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	bin := filepath.Join(t.TempDir(), "trebi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/x/bin")
	if err := NewService(agents).Install(InstallOpts{Bin: bin}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DaemonPlistName, TrayPlistName} {
		data, err := os.ReadFile(filepath.Join(agents, name))
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, string(data), "<string>/x/bin:")
		mustContain(t, string(data), ":/opt/homebrew/bin:")
	}
}

func TestRefreshPath(t *testing.T) {
	agents := t.TempDir()
	svc := NewService(agents)
	t.Setenv("PATH", "/x/bin")
	homeDir, _ := os.UserHomeDir()
	want := UserPath(homeDir, "/x/bin")
	for _, def := range []PlistDef{DaemonAgent(true), TrayAgent()} {
		body := def.Render("/app/trebi", "/usr/bin:/bin", "/Users/me")
		if err := os.WriteFile(filepath.Join(agents, def.FileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := svc.RefreshPath()
	if err != nil || !changed {
		t.Fatalf("first refresh = %v, %v; want true, nil", changed, err)
	}
	for _, name := range []string{DaemonPlistName, TrayPlistName} {
		data, err := os.ReadFile(filepath.Join(agents, name))
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, string(data), "<string>"+want+"</string>")
		mustContain(t, string(data), "<string>/Users/me</string>")
	}
	changed, err = svc.RefreshPath()
	if err != nil || changed {
		t.Fatalf("second refresh = %v, %v; want false, nil", changed, err)
	}
	if changed, err := NewService(t.TempDir()).RefreshPath(); err != nil || changed {
		t.Fatalf("no plists = %v, %v; want false, nil", changed, err)
	}
}
