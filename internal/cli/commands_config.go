package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

func configCommand() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "inspect and edit trebi config",
		Subcommands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show effective config (API when daemon up)",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.BoolFlag{Name: "yaml", Usage: "emit YAML"},
					&cli.BoolFlag{Name: "tree", Usage: "nested YAML tree"},
					&cli.StringSliceFlag{Name: "key", Aliases: []string{"k"}, Usage: "filter key path or prefix (repeatable)"},
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
				Action: func(c *cli.Context) error {
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
				Action: func(c *cli.Context) error {
					fmt.Fprintln(outWriter(c), config.ConfigPath())
					return nil
				},
			},
			{
				Name:  "bridge",
				Usage: "harness resource inheritance (skills, MCP, …)",
				Subcommands: []*cli.Command{
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
		// Default action: show (compat with old `trebi config`)
		Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON (default show)"}},
		Action: runConfigShow,
	}
}

func runConfigShow(c *cli.Context) error {
	format, err := configShowFormat(c)
	if err != nil {
		return err
	}
	body, err := loadEffectiveConfig(appStateFor(c))
	if err != nil {
		return err
	}
	keys := c.StringSlice("key")
	if len(keys) > 0 && format != "" {
		body = filterConfigKeysNested(body, keys)
	}
	w := outWriter(c)
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	case "yaml", "tree":
		// Nested YAML for both; --tree keeps map shape (not flat key list).
		data, err := yaml.Marshal(body)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	default:
		renderConfigTable(w, body, keys)
		return nil
	}
}

// configShowFormat validates the mutually exclusive output flags.
func configShowFormat(c *cli.Context) (string, error) {
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

// loadEffectiveConfig prefers GET /v1/config; falls back to local config.yaml.
func loadEffectiveConfig(state *appState) (map[string]any, error) {
	client, err := state.client, state.clientErr
	var body map[string]any
	if err == nil && client != nil {
		if gerr := client.GetJSON("/v1/config", &body); gerr != nil {
			err = gerr
		}
	}
	if err != nil || client == nil {
		// Local fallback
		if state.configErr != nil {
			return nil, state.configErr
		}
		raw, marshalErr := json.Marshal(state.config)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if unmarshalErr := json.Unmarshal(raw, &body); unmarshalErr != nil {
			return nil, unmarshalErr
		}
	}
	if body == nil {
		body = map[string]any{}
	}
	// API wraps as {"config":{…}}; local load is flat.
	return unwrapConfigMap(body), nil
}

func renderConfigTable(w io.Writer, body map[string]any, keys []string) {
	tbl := newTable(w, []string{"Key", "Value"})
	if len(keys) > 0 {
		flat := map[string]string{}
		collectFlat("", body, flat)
		for _, k := range keys {
			for fk, fv := range flat {
				if fk == k || strings.HasPrefix(fk, k+".") {
					tbl.AppendRow(table.Row{fk, fv})
				}
			}
		}
	} else {
		flattenConfig(tbl, "", body)
	}
	renderTable(tbl)
}

// filterConfigKeysNested keeps only subtrees matching key paths or prefixes.
func filterConfigKeysNested(body map[string]any, keys []string) map[string]any {
	flat := map[string]string{}
	collectFlat("", body, flat)
	out := map[string]any{}
	for _, k := range keys {
		// Prefer full nested object when key is an exact map path.
		if sub, ok := lookupNested(body, k); ok {
			setNested(out, k, sub)
			continue
		}
		for fk, fv := range flat {
			if fk == k || strings.HasPrefix(fk, k+".") {
				setNested(out, fk, fv)
			}
		}
	}
	return out
}

func lookupNested(body map[string]any, dotted string) (any, bool) {
	parts := strings.Split(dotted, ".")
	var cur any = body
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func setNested(out map[string]any, dotted string, val any) {
	parts := strings.Split(dotted, ".")
	cur := out
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = val
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
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

func collectFlat(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			collectFlat(key, vv, out)
		}
	default:
		if prefix != "" {
			out[prefix] = fmt.Sprint(x)
		}
	}
}

func runConfigSet(c *cli.Context) error {
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
		fmt.Fprintf(outWriter(c), "set %s (local)\n", key)
		return nil
	}
	if err := state.client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "set %s\n", key)
	return nil
}

func runConfigBridgeStatus(c *cli.Context) error {
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

func runConfigBridgeSet(c *cli.Context) error {
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

func applyConfigHTTPOrLocal(c *cli.Context, state *appState, req HTTPRequest, key, value string) error {
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

func flattenConfig(tbl table.Writer, prefix string, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenConfig(tbl, key, x[k])
		}
	case []any:
		b, _ := json.Marshal(x)
		tbl.AppendRow(table.Row{prefix, string(b)})
	default:
		if prefix == "" {
			return
		}
		tbl.AppendRow(table.Row{prefix, fmt.Sprint(x)})
	}
}
