package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestRenderPlist_AbsoluteProgramArguments(t *testing.T) {
	t.Parallel()
	// Default install: tray + OnFailure KeepAlive (no --no-tray).
	def := DaemonAgent(false)
	def.LogPath = "/tmp/trebid.log"
	bin := "/Users/me/Applications/Trebi.app/Contents/MacOS/trebi"
	got := def.Render(bin, "/usr/bin:/bin", "/Users/me")

	mustContain(t, got, "<string>ai.trebi.daemon</string>")
	mustContain(t, got, "<string>/Users/me/Applications/Trebi.app/Contents/MacOS/trebi</string>")
	mustContain(t, got, "<string>service</string>")
	mustContain(t, got, "<string>daemon</string>")
	mustContain(t, got, "<string>127.0.0.1:0</string>")
	mustContain(t, got, "<string>/Users/me</string>")
	mustContain(t, got, "<string>/tmp/trebid.log</string>")
	mustContain(t, got, "<key>SuccessfulExit</key>")
	mustContain(t, got, "<key>RunAtLoad</key>\n    <true/>")
	if strings.Contains(got, "--no-tray") {
		t.Fatalf("tray install must not pass --no-tray:\n%s", got)
	}

	// Absolute path must appear before subcommands.
	idxBin := strings.Index(got, bin)
	idxSvc := strings.Index(got, "<string>service</string>")
	if idxBin < 0 || idxSvc < 0 || idxBin > idxSvc {
		t.Fatalf("absolute bin must precede service args\n%s", got)
	}

	headless := DaemonAgent(true)
	gotH := headless.Render(bin, "/usr/bin:/bin", "/Users/me")
	mustContain(t, gotH, "<string>--no-tray</string>")
	mustContain(t, gotH, "<key>KeepAlive</key>\n    <true/>")
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
	def := DaemonAgent(true)
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
	if !contains(trebid, "--no-tray") {
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
	tray := DaemonAgent(false)
	if tray.Label != DaemonLabel {
		t.Fatalf("label: %s", tray.Label)
	}
	if tray.FileName != DaemonPlistName {
		t.Fatalf("file: %s", tray.FileName)
	}
	if tray.KeepAlive {
		t.Fatal("tray agent: want KeepAlive OnFailure (false)")
	}
	if contains(tray.Args, "--no-tray") {
		t.Fatalf("tray args must omit --no-tray: %v", tray.Args)
	}
	headless := DaemonAgent(true)
	if !headless.KeepAlive {
		t.Fatal("headless: want KeepAlive always")
	}
	if !contains(headless.Args, "--no-tray") || !contains(headless.Args, "service") {
		t.Fatalf("headless args: %v", headless.Args)
	}
	if !strings.HasSuffix(headless.LogPath, filepath.Join("logs", "trebid.log")) {
		t.Fatalf("log path: %s", headless.LogPath)
	}
}

func TestFormatStatus(t *testing.T) {
	t.Parallel()
	running := FormatStatus("123\t0\tai.trebi.daemon\nother", true)
	if !strings.HasPrefix(running, DaemonLabel+": Running") {
		t.Fatalf("running: %s", running)
	}
	installed := FormatStatus("no match here", true)
	if installed != DaemonLabel+": Not running (plist installed)" {
		t.Fatalf("installed: %s", installed)
	}
	missing := FormatStatus("", false)
	if missing != DaemonLabel+": Not installed" {
		t.Fatalf("missing: %s", missing)
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
	// Default install includes tray (no --no-tray).
	if strings.Contains(body, "--no-tray") {
		t.Fatal("default install should enable tray")
	}
	mustContain(t, body, home.DaemonLogPath())

	// Uninstall removes the test plist only.
	if err := svc.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plist should be removed, err=%v", err)
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
