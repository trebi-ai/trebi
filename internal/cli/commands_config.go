package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

func configCommand() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "inspect and edit trebi config",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show effective config (API when daemon up)",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{
						Name:    "key",
						Aliases: []string{"k"},
						Usage:   "Filter by key path or glob (repeatable); e.g. tools.grok (section), defaults.command (exact), or defaults.*",
					},
					&cli.BoolFlag{Name: "json", Usage: "JSON array of {key,value,source,default,description}"},
					&cli.BoolFlag{Name: "yaml", Usage: "YAML list of key entries"},
					&cli.BoolFlag{Name: "tree", Usage: "Nested tree (value/source/default leaves)"},
				},
				Action: runConfigShow,
			},
			{
				Name:      "set",
				Usage:     "set a dotted config key via PATCH /v1/config",
				ArgsUsage: "<key> <value>",
				Action:    runConfigSet,
			},
			{
				Name:  "validate",
				Usage: "validate local config.yaml (no daemon required)",
				Action: func(_ context.Context, c *cli.Command) error {
					cfg, err := configFor(c)
					if err != nil {
						return err
					}
					if err := config.Validate(cfg); err != nil {
						return err
					}
					fmt.Fprintln(outWriter(c), "ok")
					return nil
				},
			},
			{
				Name:  "path",
				Usage: "print path to config.yaml",
				Action: func(_ context.Context, c *cli.Command) error {
					fmt.Fprintln(outWriter(c), config.ConfigPath())
					return nil
				},
			},
			{
				Name:  "bridge",
				Usage: "harness resource inheritance (skills, MCP, …)",
				Commands: []*cli.Command{
					{
						Name:      "status",
						Usage:     "show bridge policy from local config",
						ArgsUsage: "[tool]",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action:    runConfigBridgeStatus,
					},
					{
						Name:      "set",
						Usage:     "set bridge mode: <tool> <resource> <inherit|false> | --global never <resource>",
						ArgsUsage: "<tool> <resource> <mode> | --global never <resource>",
						Flags: []cli.Flag{
							&cli.BoolFlag{Name: "global", Usage: "add resource to tools.bridge.never"},
						},
						Action: runConfigBridgeSet,
					},
				},
			},
		},
	}
}

func runConfigShow(_ context.Context, c *cli.Command) error {
	format, err := configShowFormat(c)
	if err != nil {
		return err
	}
	entries, err := loadConfigEntries(appStateFor(c))
	if err != nil {
		return err
	}
	entries, err = config.FilterPathEntries(entries, c.StringSlice("key"))
	if err != nil {
		return err
	}
	w := outWriter(c)
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "yaml":
		data, err := yaml.Marshal(entries)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	case "tree":
		// Nested map shape; each leaf keeps value/source/default/description.
		data, err := yaml.Marshal(config.EntriesToTreeMap(entries))
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	default:
		renderConfigTable(w, entries)
		return nil
	}
}

// configShowFormat validates the mutually exclusive output flags.
func configShowFormat(c *cli.Command) (string, error) {
	var formats []string
	for _, f := range []string{"json", "yaml", "tree"} {
		if c.Bool(f) {
			formats = append(formats, f)
		}
	}
	switch len(formats) {
	case 0:
		return "", nil
	case 1:
		return formats[0], nil
	default:
		return "", fmt.Errorf("use only one of --json, --yaml, --tree")
	}
}

// loadConfigEntries prefers the daemon's entries so a remote CLI reports the
// server config, not its own file. Falls back to the local config.yaml.
func loadConfigEntries(state *appState) ([]config.PathEntry, error) {
	if state.clientErr == nil && state.client != nil {
		var body struct {
			Entries []config.PathEntry `json:"entries"`
		}
		if err := state.client.GetJSON("/v1/config", &body); err == nil && len(body.Entries) > 0 {
			return body.Entries, nil
		}
	}
	cfg, declared, err := config.LoadWithDeclared()
	if err != nil {
		return nil, err
	}
	return config.ListPathEntries(cfg, declared), nil
}

// configValueWidth caps the Value column. Long notify commands would otherwise
// wrap over several lines; --json/--yaml/--tree still carry the full value.
const configValueWidth = 40

func renderConfigTable(w io.Writer, entries []config.PathEntry) {
	tbl := newTable(w, []string{"Key", "Value", "Source", "Default", "Description"})
	for _, e := range entries {
		desc := e.Description
		if desc == "" {
			desc = "-"
		}
		tbl.AppendRow(table.Row{e.Key, truncateDisplay(e.Value, configValueWidth), e.Source, e.Default, desc})
	}
	renderTable(tbl)
}

// truncateDisplay shortens s to max display runes, ending with "...".
// It counts runes, not bytes, so a multi-byte character is never split.
func truncateDisplay(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// unwrapConfigMap peels GET /v1/config {"config":{…}} so keys match local defaults.*.
func unwrapConfigMap(body map[string]any) map[string]any {
	if body == nil {
		return map[string]any{}
	}
	if inner, ok := body["config"].(map[string]any); ok {
		return inner
	}
	return body
}

func runConfigSet(_ context.Context, c *cli.Command) error {
	if c.NArg() < 2 {
		return fmt.Errorf("usage: trebi config set <key> <value>")
	}
	key := c.Args().Get(0)
	value := strings.Join(c.Args().Tail(), " ")
	req := BuildConfigSet(key, value)
	state := appStateFor(c)
	if state.clientErr != nil || state.client == nil {
		// Local set when daemon down
		if state.configErr != nil {
			return state.configErr
		}
		if err := config.SetPath(state.config, key, value); err != nil {
			return err
		}
		if err := state.config.Save(); err != nil {
			return err
		}
		printConfigKey(c, state, key, " (local)")
		return nil
	}
	if err := state.client.DoHTTP(req, nil); err != nil {
		return err
	}
	printConfigKey(c, state, key, "")
	return nil
}

// printConfigKey echoes the effective row after a set, so the caller sees the
// value the daemon actually holds. Falls back to the key name on a read error.
func printConfigKey(c *cli.Command, state *appState, key, suffix string) {
	w := outWriter(c)
	entries, err := loadConfigEntries(state)
	if err == nil {
		for _, e := range entries {
			if e.Key == key {
				fmt.Fprintf(w, "%s = %s (%s)%s\n", e.Key, e.Value, e.Source, suffix)
				return
			}
		}
	}
	fmt.Fprintf(w, "set %s%s\n", key, suffix)
}

func runConfigBridgeStatus(_ context.Context, c *cli.Command) error {
	cfg, err := configFor(c)
	if err != nil {
		return err
	}
	tool := c.Args().Get(0)
	out := map[string]any{
		"global": cfg.Tools.Bridge,
	}
	if tool != "" {
		if pin := cfg.Tools.Pin(tool); pin != nil {
			out["tool"] = tool
			out["bridge"] = pin.Bridge
			out["auth"] = pin.Auth
		} else {
			out["tool"] = tool
			out["bridge"] = nil
		}
	} else {
		tools := map[string]any{}
		for _, name := range config.FirstClassTools() {
			if pin := cfg.Tools.Pin(name); pin != nil && pin.Bridge != nil {
				tools[name] = pin.Bridge
			}
		}
		out["tools"] = tools
	}
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func runConfigBridgeSet(_ context.Context, c *cli.Command) error {
	state := appStateFor(c)
	cfg, err := configFor(c)
	if err != nil {
		return err
	}
	if c.Bool("global") {
		// --global never <resource> — append, do not replace.
		if c.NArg() < 2 || c.Args().Get(0) != "never" {
			return fmt.Errorf("usage: trebi config bridge set --global never <resource>")
		}
		resource := c.Args().Get(1)
		joined, err := mergeBridgeNever(cfg, state.client, resource)
		if err != nil {
			return err
		}
		req := BuildConfigSet("tools.bridge.never", joined)
		return applyConfigHTTPOrLocal(c, state, req, "tools.bridge.never", joined)
	}
	if c.NArg() < 3 {
		return fmt.Errorf("usage: trebi config bridge set <tool> <resource> <inherit|false>")
	}
	tool, resource, mode := c.Args().Get(0), c.Args().Get(1), c.Args().Get(2)
	req, err := BuildConfigBridgeSet(tool, resource, mode)
	if err != nil {
		return err
	}
	key := "tools." + tool + ".bridge.resources." + resource
	return applyConfigHTTPOrLocal(c, state, req, key, mode)
}

// mergeBridgeNever reads current tools.bridge.never and appends resource if missing.
func mergeBridgeNever(cfg *config.Config, client *APIClient, resource string) (string, error) {
	var list []string
	if cfg.Tools.Bridge != nil {
		list = append(list, cfg.Tools.Bridge.Never...)
	}
	// Also try API when available (daemon may have newer state).
	if client != nil {
		var body map[string]any
		if err := client.GetJSON("/v1/config", &body); err == nil {
			doc := unwrapConfigMap(body)
			if tools, ok := doc["tools"].(map[string]any); ok {
				if br, ok := tools["bridge"].(map[string]any); ok {
					switch n := br["never"].(type) {
					case []any:
						list = nil
						for _, x := range n {
							list = append(list, fmt.Sprint(x))
						}
					case []string:
						list = n
					}
				}
			}
		}
	}
	for _, x := range list {
		if x == resource {
			return strings.Join(list, ","), nil
		}
	}
	list = append(list, resource)
	return strings.Join(list, ","), nil
}

func applyConfigHTTPOrLocal(c *cli.Command, state *appState, req HTTPRequest, key, value string) error {
	if state.clientErr != nil || state.client == nil {
		if state.configErr != nil {
			return state.configErr
		}
		cfg := state.config
		// Prefer SetPath for dotted keys
		if sets, ok := req.Body.(map[string]any); ok {
			if setMap, ok := sets["set"].(map[string]any); ok {
				for k, v := range setMap {
					if err := config.SetPath(cfg, k, fmt.Sprint(v)); err != nil {
						return err
					}
					key = k
					value = fmt.Sprint(v)
				}
			}
		} else if err := config.SetPath(cfg, key, value); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "set %s=%s (local)\n", key, value)
		return nil
	}
	if err := state.client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "set %s\n", key)
	return nil
}
