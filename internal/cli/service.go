package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/api"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tray"
	"github.com/urfave/cli/v3"
)

// errIfRemoteSticky blocks local-only service commands when sticky context is remote.
func (a *appState) errIfRemoteSticky() error {
	if a.stickyErr != nil {
		return a.stickyErr
	}
	if a.sticky != nil && a.sticky.IsRemote() {
		return fmt.Errorf("service commands are local-only (sticky workspace is remote %s); trebi workspace use <local-id>", a.sticky.URL)
	}
	return nil
}

func serviceCommand() *cli.Command {
	return &cli.Command{
		Name:  "service",
		Usage: "daemon lifecycle",
		Commands: slices.Concat(
			serviceLifecycleSubcommands(),
			serviceDiagnosticsSubcommands(),
			serviceDaemonSubcommands(),
			serviceTokenSubcommands(),
			serviceCloudSubcommands(),
		),
	}
}

// statusCommand is the one-call operator view: "is trebi ready, and what needs
// me?". It degrades to the OS service status when the daemon is down.
func statusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "one-screen overview: health, runs, inbox, next due",
		Flags: []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
		Action: func(_ context.Context, c *cli.Command) error {
			client, err := clientFor(c)
			if err != nil {
				return runServiceStatus(context.Background(), c)
			}
			var body map[string]any
			if err := client.GetJSON(request.BuildSystemOverviewPath(), &body); err != nil {
				return runServiceStatus(context.Background(), c)
			}
			if c.Bool("json") {
				return encodeJSON(c, body)
			}
			return printOverview(outWriter(c), body)
		},
	}
}

func printOverview(w io.Writer, body map[string]any) error {
	runs, _ := body["runs"].(map[string]any)
	inbox, _ := body["inbox"].(map[string]any)
	doctor, _ := body["doctor"].(map[string]any)
	tbl := newTable(w, []string{"Field", "Value"})
	for _, row := range [][2]any{
		{"version", body["version"]},
		{"workspaces", body["workspaces"]},
		{"running", runs["running"]},
		{"waiting", runs["waiting"]},
		{"inbox unread", inbox["unread"]},
		{"approvals", inbox["approvals"]},
		{"doctor warn", doctor["warn"]},
		{"doctor error", doctor["error"]},
		{"next due", dash(fmt.Sprint(body["next_due_at"]))},
	} {
		tbl.AppendRow(table.Row{row[0], row[1]})
	}
	renderTable(tbl)
	return nil
}

// serviceLifecycleSubcommands returns the OS-supervisor commands: install, uninstall, start, stop, restart.
func serviceLifecycleSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "install",
			Usage: "Install launchd LaunchAgent (macOS) for trebid",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:  "no-tray",
					Usage: "Headless daemon (no menu-bar tray; KeepAlive always)",
				},
				&cli.StringFlag{
					Name:  "bin",
					Usage: "Absolute path to trebi/trebid (default: resolve TREBI_BIN, sibling trebid, or PATH)",
				},
			},
			Action: runServiceInstall,
		},
		{
			Name:  "uninstall",
			Usage: "Remove launchd LaunchAgent",
			Action: func(_ context.Context, c *cli.Command) error {
				if err := appStateFor(c).errIfRemoteSticky(); err != nil {
					return err
				}
				return appStateFor(c).platform.Uninstall()
			},
		},
		{
			Name:  "start",
			Usage: "Start the OS-supervised daemon",
			Action: func(_ context.Context, c *cli.Command) error {
				if err := appStateFor(c).errIfRemoteSticky(); err != nil {
					return err
				}
				if err := appStateFor(c).platform.Start(); err != nil {
					return err
				}
				fmt.Println("Service started.")
				return nil
			},
		},
		{
			Name:  "stop",
			Usage: "Stop the OS-supervised daemon",
			Action: func(_ context.Context, c *cli.Command) error {
				if err := appStateFor(c).errIfRemoteSticky(); err != nil {
					return err
				}
				if err := appStateFor(c).platform.Stop(); err != nil {
					return err
				}
				fmt.Println("Service stopped.")
				return nil
			},
		},
		{
			Name:  "restart",
			Usage: "Restart the OS-supervised daemon",
			Action: func(_ context.Context, c *cli.Command) error {
				if err := appStateFor(c).platform.Restart(); err != nil {
					return err
				}
				fmt.Println("Service restarted.")
				return nil
			},
		},
	}
}

// serviceDiagnosticsSubcommands returns status, logs, and migrate-db.
func serviceDiagnosticsSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "status",
			Usage: "Show OS service status and API health when available",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runServiceStatus,
		},
		{
			Name:  "version",
			Usage: "Show the running version and whether an upgrade is available",
			Flags: []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action: func(_ context.Context, c *cli.Command) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				var body map[string]any
				if err := client.GetJSON(request.BuildSystemUpdatePath(), &body); err != nil {
					return err
				}
				return encodeJSON(c, body)
			},
		},
		{
			Name:    "logs",
			Aliases: []string{"log"},
			Usage:   "View the daemon log (launchd stdout/stderr → trebid.log)",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "Follow log output"},
				&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: 50, Usage: "Number of recent lines"},
				&cli.StringFlag{Name: "level", Usage: "Only lines containing this level (debug|info|warn|error)"},
				&cli.StringFlag{Name: "grep", Usage: "Only lines containing this substring (case-insensitive)"},
				&cli.BoolFlag{Name: "json", Usage: "Print raw lines without filtering notes"},
			},
			Action: runServiceLogs,
		},
		{
			Name:   "migrate-db",
			Usage:  "create/migrate trebi.db schema and exit",
			Action: runServiceMigrateDB,
		},
	}
}

// serviceDaemonSubcommands returns daemon, drain, and undrain.
func serviceDaemonSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "daemon",
			Usage: "run trebid (HTTP API + workers; menu-bar tray on macOS unless --no-tray)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "listen", Usage: "HTTP listen address (overrides api.host:api.port)"},
				&cli.BoolFlag{Name: "shadow", Usage: "shadow mode: drain + no connectors/schedules"},
				&cli.BoolFlag{Name: "no-tray", Usage: "disable menu-bar tray (headless)"},
				&cli.IntFlag{
					Name:    "parent-pid",
					Sources: cli.EnvVars("TREBI_PARENT_PID"),
					Usage:   "exit when this pid dies (eval harness; also TREBI_PARENT_FD pipe)",
				},
				&cli.StringFlag{
					Name:    "log-level",
					Sources: cli.EnvVars("TREBI_LOG_LEVEL"),
					Value:   "info",
					Usage:   "debug|info|warn|error (debug shows tray health polls)",
				},
			},
			Action: runDaemonProcess,
		},
		{
			Name:  "drain",
			Usage: "enter drain mode",
			Action: func(_ context.Context, c *cli.Command) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return client.DoHTTP(request.BuildSystemDrain(), nil)
			},
		},
		{
			Name:  "undrain",
			Usage: "leave drain mode",
			Action: func(_ context.Context, c *cli.Command) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return client.DoHTTP(request.BuildSystemUndrain(), nil)
			},
		},
		{
			Name:  "shutdown",
			Usage: "ask the daemon to shut down",
			Flags: []cli.Flag{forceFlag("required; scheduled jobs stop firing")},
			Action: func(_ context.Context, c *cli.Command) error {
				confirm, err := confirmed(c, "stops the daemon; scheduled jobs stop firing until it starts again")
				if err != nil {
					return err
				}
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return client.DoHTTP(request.BuildServiceShutdown(confirm), nil)
			},
		},
	}
}

// serviceCloudSubcommands returns the cloudlink command group.
func serviceCloudSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "cloud",
			Usage: "cloudlink status and pairing",
			Commands: []*cli.Command{
				{
					Name:  "status",
					Usage: "show the cloud link state",
					Flags: []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
					Action: func(_ context.Context, c *cli.Command) error {
						client, err := clientFor(c)
						if err != nil {
							return err
						}
						var body map[string]any
						if err := client.GetJSON(request.BuildCloudStatusPath(), &body); err != nil {
							return err
						}
						return encodeJSON(c, body)
					},
				},
				{
					Name:  "link",
					Usage: "link this daemon to the cloud service",
					Action: func(_ context.Context, c *cli.Command) error {
						client, err := clientFor(c)
						if err != nil {
							return err
						}
						var body map[string]any
						if err := client.DoHTTP(request.BuildCloudLink(), &body); err != nil {
							return err
						}
						return encodeJSON(c, body)
					},
				},
				{
					Name:  "unlink",
					Usage: "unlink this daemon from the cloud service",
					Flags: []cli.Flag{forceFlag("required; remote clients lose access")},
					Action: func(_ context.Context, c *cli.Command) error {
						confirm, err := confirmed(c, "unlinks this daemon from the cloud; remote clients lose access")
						if err != nil {
							return err
						}
						client, err := clientFor(c)
						if err != nil {
							return err
						}
						return client.DoHTTP(request.BuildCloudUnlink(confirm), nil)
					},
				},
			},
		},
	}
}

// serviceTokenSubcommands returns the token command group.
func serviceTokenSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "token",
			Usage: "token management",
			Commands: []*cli.Command{
				{
					Name:  "show",
					Usage: "print current API token",
					Action: func(_ context.Context, c *cli.Command) error {
						client, err := clientFor(c)
						if err != nil {
							return err
						}
						fmt.Println(client.token)
						return nil
					},
				},
			},
		},
	}
}

func runServiceInstall(_ context.Context, c *cli.Command) error {
	if err := appStateFor(c).errIfRemoteSticky(); err != nil {
		return err
	}
	return appStateFor(c).platform.Install(platform.InstallOpts{
		NoTray: c.Bool("no-tray"),
		Bin:    c.String("bin"),
	})
}

func runServiceStatus(_ context.Context, c *cli.Command) error {
	osStatus := ""
	osErr := ""
	st, err := appStateFor(c).platform.Status()
	if err != nil {
		osErr = err.Error()
	} else {
		osStatus = st
	}
	var apiBody map[string]any
	apiErr := ""
	client, err := clientFor(c)
	if err != nil {
		apiErr = err.Error()
	} else if err := client.GetJSON(request.BuildSystemHealthPath(), &apiBody); err != nil {
		apiErr = err.Error()
	}
	if c.Bool("json") {
		out := map[string]any{}
		if osStatus != "" {
			out["os"] = osStatus
		}
		if osErr != "" {
			out["os_error"] = osErr
		}
		if apiErr != "" {
			out["api_error"] = apiErr
		} else {
			out["api"] = apiBody
		}
		return encodeJSON(c, out)
	}
	if osErr != "" {
		fmt.Fprintf(c.Root().ErrWriter, "os: %v\n", osErr)
	} else {
		fmt.Println(osStatus)
	}
	if apiErr != "" {
		fmt.Printf("api: unreachable (%v)\n", apiErr)
		return nil
	}
	fmt.Printf("api: status=%v version=%v drain=%v\n", apiBody["status"], apiBody["version"], apiBody["drain_state"])
	return nil
}

func runServiceLogs(_ context.Context, c *cli.Command) error {
	logFile := home.DaemonLogPath()
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s (start the service once)", logFile)
	}
	args := []string{"-n", fmt.Sprintf("%d", c.Int("lines"))}
	if c.Bool("follow") {
		args = append(args, "-f")
	}
	args = append(args, logFile)
	cmd := exec.Command("tail", args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	streamServiceLog(c.Root().Writer, stdout, c.String("level"), c.String("grep"), c.Bool("json"))
	return cmd.Wait()
}

func runServiceMigrateDB(_ context.Context, _ *cli.Command) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	v, err := st.SchemaVersion()
	if err != nil {
		return err
	}
	fmt.Printf("schema_version=%d path=%s\n", v, st.Path())
	return nil
}

// streamServiceLog filters daemon log lines (plain or slog JSON).
func streamServiceLog(w io.Writer, r io.Reader, level, grep string, raw bool) {
	level = strings.ToUpper(strings.TrimSpace(level))
	grep = strings.ToLower(grep)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if level != "" {
			if !lineHasLevel(line, level) {
				continue
			}
		}
		if grep != "" && !strings.Contains(strings.ToLower(line), grep) {
			continue
		}
		_ = raw // raw kept for flag parity; lines are already plain/JSON as written
		fmt.Fprintln(w, line)
	}
}

func lineHasLevel(line, want string) bool {
	// slog JSON: "level":"INFO" or plain text containing INFO
	if strings.Contains(strings.ToUpper(line), `"LEVEL":"`+want+`"`) {
		return true
	}
	if strings.Contains(strings.ToUpper(line), want) {
		return true
	}
	var raw map[string]any
	if json.Unmarshal([]byte(line), &raw) == nil {
		if lv, ok := raw["level"].(string); ok && strings.EqualFold(lv, want) {
			return true
		}
	}
	return false
}

// runDaemonProcess starts the HTTP control plane and, on macOS, the menu-bar tray.
// Tray icon means the service process is alive. Quit from the tray exits 0 and
// stops the whole process (daemon + tray).
func runDaemonProcess(ctx context.Context, c *cli.Command) error {
	if err := appStateFor(c).errIfRemoteSticky(); err != nil {
		return err
	}
	noTray := c.Bool("no-tray") || runtime.GOOS != "darwin" || !tray.Available
	if noTray {
		if !c.Bool("no-tray") && runtime.GOOS == "darwin" && !tray.Available {
			appStateFor(c).log.Warn("tray unavailable (need cgo on darwin); running headless")
		}
		return runDaemon(ctx, c)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		err := runDaemon(ctx, c)
		errCh <- err
		cancel() // daemon died → tear down tray
	}()

	// Menuet owns the main thread.
	trayErr := tray.Run(ctx, cancel)
	cancel()

	// Give the daemon a moment to shut down after tray quit / cancel.
	select {
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			return err
		}
	case <-time.After(5 * time.Second):
	}
	return trayErr
}

// setLogLevel installs the default slog handler at the requested level.
// Routine tray polls log at debug, so "debug" is what surfaces them.
func setLogLevel(name string) error {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		level = slog.LevelInfo
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("log-level %q: want debug|info|warn|error", name)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return nil
}

func runDaemon(parent context.Context, c *cli.Command) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	lock, err := home.AcquireDaemonLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := setLogLevel(c.String("log-level")); err != nil {
		return err
	}
	// First run seeds config.yaml so there is a real file to edit. Defaults
	// still apply when it is absent; this only removes the "missing" state.
	if created, err := config.Seed(); err != nil {
		appStateFor(c).log.Warn("config seed", "error", err)
	} else if created {
		fmt.Fprintf(os.Stderr, "wrote default config → %s\n", config.ConfigPath())
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	drainFlag, _, _ := st.GetFlag("drain")
	shadow := c.Bool("shadow")
	token := daemonToken()
	drain := drainFlag == "1" || shadow
	cp := daemon.New(st, daemon.Options{Shadow: shadow, Drain: drain})
	srv := api.New(st, api.Options{
		Token:  token,
		Shadow: shadow,
		Drain:  drain,
	})
	// Share control-plane services so API and workers see the same state.
	srv.BindControlPlane(cp.Runs, cp.Workspaces, cp.Connectors, cp.Schedule, cp.Alerts, cp.Files, cp.Conversations, cp.Harness, cp.Email, cp.License)
	srv.Skills().WithUserHome(harness.UserHome())

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	startParentWatch(cancel, c.Int("parent-pid"))
	cp.Start(ctx)

	bind, err := resolveDaemonBind(c.String("listen"))
	if err != nil {
		cp.Stop()
		return err
	}
	var addr string
	if bind.Cert != "" {
		addr, err = srv.ListenAndServeTLS(bind.Addr, bind.Cert, bind.Key)
	} else {
		addr, err = srv.ListenAndServe(bind.Addr)
	}
	if err != nil {
		cp.Stop()
		return err
	}
	if err := srv.WriteAPIJSON(addr); err != nil {
		cp.Stop()
		return err
	}
	appStateFor(c).appendBusEvent(st, "system.started")
	fmt.Fprintf(os.Stderr, "trebid listening on %s://%s (home=%s shadow=%v tray=%v)\n",
		bind.Scheme, addr, home.Base(), shadow, !c.Bool("no-tray") && tray.Available && runtime.GOOS == "darwin")
	fmt.Fprintf(os.Stderr, "api.json → %s\n", home.APIJSONPath())

	_ = os.WriteFile(filepath.Join(home.Base(), "trebid.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-ch:
	case <-ctx.Done():
	}
	appStateFor(c).appendBusEvent(st, "system.stopping")
	cp.Stop()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

type daemonBind struct {
	Addr   string
	Cert   string
	Key    string
	Scheme string
}

// resolveDaemonBind prefers --listen; otherwise api.host:api.port. Non-loopback
// requires TLS files or api.tls.insecure (B28).
func resolveDaemonBind(flag string) (daemonBind, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonBind{}, err
	}
	addr := strings.TrimSpace(flag)
	if addr == "" {
		addr = cfg.API.ListenAddr()
	}
	if err := config.BindGuard(addr, cfg.API); err != nil {
		return daemonBind{}, err
	}
	cert, key, err := cfg.API.TLSFiles()
	if err != nil {
		return daemonBind{}, err
	}
	if cert != "" {
		if _, err := os.Stat(cert); err != nil {
			return daemonBind{}, fmt.Errorf("api.tls.cert: %w", err)
		}
		if _, err := os.Stat(key); err != nil {
			return daemonBind{}, fmt.Errorf("api.tls.key: %w", err)
		}
	}
	scheme := "http"
	if cert != "" {
		scheme = "https"
	}
	return daemonBind{Addr: addr, Cert: cert, Key: key, Scheme: scheme}, nil
}

// daemonToken reuses the token from api.json when present, else mints one.
func daemonToken() string {
	token := ulid.Make().String()
	if data, err := os.ReadFile(home.APIJSONPath()); err == nil {
		var doc struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(data, &doc) == nil && doc.Token != "" {
			token = doc.Token
		}
	}
	return token
}

func (s *appState) appendBusEvent(st *store.Store, topic string) {
	if _, err := st.Append(context.Background(), store.BusEvent{Topic: topic}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
}

const parentFDEnv = "TREBI_PARENT_FD"

// startParentWatch cancels when the eval (or other) parent dies, including
// SIGKILL: the parent holds the write end of a pipe passed as ExtraFiles.
func startParentWatch(cancel context.CancelFunc, parentPID int) {
	if parentPID <= 0 {
		parentPID, _ = strconv.Atoi(os.Getenv("TREBI_PARENT_PID"))
	}
	var fd *os.File
	if v := os.Getenv(parentFDEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 3 {
			fd = os.NewFile(uintptr(n), "parent-watch")
		}
	}
	if parentPID <= 0 && fd == nil {
		return
	}
	go watchParent(cancel, parentPID, fd)
}

func watchParent(cancel context.CancelFunc, pid int, fd *os.File) {
	if fd != nil {
		buf := make([]byte, 1)
		_, _ = fd.Read(buf)
		cancel()
		return
	}
	if pid <= 0 {
		return
	}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if !processAlive(pid) {
			cancel()
			return
		}
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
