package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/urfave/cli/v3"
)

func mcpCommand() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "connect an MCP client to this daemon",
		Commands: []*cli.Command{
			{
				Name:  "config",
				Usage: "print the config block for one client",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "client", Value: "json", Usage: "claude, codex, opencode, cursor, vscode, or json"},
					&cli.BoolFlag{Name: "command", Usage: "print the add command instead of the config file block"},
				},
				Action: runMCPConfig,
			},
			{
				Name:  "install",
				Usage: "run the add command of one client",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "client", Value: "claude", Usage: "claude, codex, or opencode"},
					&cli.BoolFlag{Name: "dry-run", Usage: "print the command without running it"},
				},
				Action: runMCPInstall,
			},
			{
				Name:   "serve",
				Usage:  "bridge stdio JSON-RPC to the daemon /mcp endpoint",
				Action: runMCPServe,
			},
		},
	}
}

// mcpTarget is the daemon endpoint an MCP client must reach.
type mcpTarget struct {
	URL   string
	Token string
}

// mcpConn reads the local daemon endpoint. It is offline-capable: it reads
// api.json and never calls the daemon.
func mcpConn() (mcpTarget, error) {
	client, err := apiJSONClient()
	if err != nil {
		return mcpTarget{}, err
	}
	if client.base == "" {
		return mcpTarget{}, fmt.Errorf("api.json carries no url (is trebid running?)")
	}
	return mcpTarget{URL: client.base + "/mcp", Token: client.token}, nil
}

// block returns the config file text for one client.
func (t mcpTarget) block(client string) (string, error) {
	switch client {
	case "codex":
		return fmt.Sprintf("[mcp_servers.trebi]\nurl = %q\nbearer_token_env_var = \"TREBI_TOKEN\"\n", t.URL), nil
	case "claude":
		return t.jsonBlock(map[string]any{"mcpServers": map[string]any{"trebi": map[string]any{
			"type": "http", "url": t.URL,
			"headers": map[string]any{"Authorization": "Bearer ${TREBI_TOKEN}"},
		}}})
	case "opencode":
		return t.jsonBlock(map[string]any{"mcp": map[string]any{"servers": map[string]any{"trebi": map[string]any{
			"type": "remote", "url": t.URL, "oauth": false,
			"headers": map[string]any{"Authorization": "Bearer {env:TREBI_TOKEN}"},
		}}}})
	case "cursor":
		return t.jsonBlock(map[string]any{"mcpServers": map[string]any{"trebi": map[string]any{
			"url":     t.URL,
			"headers": map[string]any{"Authorization": "Bearer ${env:TREBI_TOKEN}"},
		}}})
	case "vscode":
		return t.jsonBlock(map[string]any{"servers": map[string]any{"trebi": map[string]any{
			"type": "http", "url": t.URL,
			"headers": map[string]any{"Authorization": "Bearer ${env:TREBI_TOKEN}"},
		}}})
	case "json", "":
		return t.jsonBlock(map[string]any{"url": t.URL, "token": t.Token})
	}
	return "", fmt.Errorf("unknown client %q; use claude, codex, opencode, cursor, vscode, or json", client)
}

func (t mcpTarget) jsonBlock(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// addCommand returns the client add command, argv style.
func (t mcpTarget) addCommand(client string) ([]string, error) {
	switch client {
	case "claude", "":
		return []string{"claude", "mcp", "add", "--transport", "http", "trebi", t.URL,
			"--header", "Authorization: Bearer " + t.Token, "--scope", "user"}, nil
	case "codex":
		return []string{"codex", "mcp", "add", "trebi", "--url", t.URL, "--bearer-token-env-var", "TREBI_TOKEN"}, nil
	case "opencode":
		return []string{"opencode2", "mcp", "add", "trebi", "--global", "--url", t.URL,
			"--header", "Authorization=Bearer " + t.Token}, nil
	}
	return nil, fmt.Errorf("client %q has no add command; use trebi mcp config --client %s", client, client)
}

func runMCPConfig(_ context.Context, c *cli.Command) error {
	target, err := mcpConn()
	if err != nil {
		return err
	}
	client := c.String("client")
	if c.Bool("command") {
		argv, err := target.addCommand(client)
		if err != nil {
			return err
		}
		fmt.Fprintln(outWriter(c), strings.Join(argv, " "))
		return nil
	}
	block, err := target.block(client)
	if err != nil {
		return err
	}
	fmt.Fprint(outWriter(c), block)
	return nil
}

func runMCPInstall(_ context.Context, c *cli.Command) error {
	target, err := mcpConn()
	if err != nil {
		return err
	}
	argv, err := target.addCommand(c.String("client"))
	if err != nil {
		return err
	}
	if c.Bool("dry-run") {
		fmt.Fprintln(outWriter(c), strings.Join(argv, " "))
		return nil
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is built from a fixed client table
	cmd.Env = append(os.Environ(), "TREBI_TOKEN="+target.Token)
	cmd.Stdout, cmd.Stderr = outWriter(c), os.Stderr
	return cmd.Run()
}

// runMCPServe bridges stdio JSON-RPC to the daemon /mcp endpoint, so a client
// that speaks stdio only reaches the same catalog.
func runMCPServe(ctx context.Context, c *cli.Command) error {
	target, err := mcpConn()
	if err != nil {
		return err
	}
	return newMCPBridge(target).run(ctx, os.Stdin, outWriter(c))
}

// mcpBridge forwards one JSON-RPC message at a time to the daemon.
type mcpBridge struct {
	target  mcpTarget
	http    *http.Client
	session string
}

func newMCPBridge(target mcpTarget) *mcpBridge {
	return &mcpBridge{target: target, http: &http.Client{}}
}

// run reads one JSON message per line and writes one reply per line. A
// notification, which carries no id, expects no reply.
func (b *mcpBridge) run(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	for {
		var msg json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		msg, err := b.resolveValueFile(msg)
		if err != nil {
			return err
		}
		reply, err := b.forward(ctx, msg)
		if err != nil {
			return err
		}
		if len(reply) == 0 {
			continue
		}
		if _, err := fmt.Fprintln(out, string(reply)); err != nil {
			return err
		}
	}
}

// resolveValueFile replaces a secret value_file argument with the file
// content. The bridge runs on the client machine, so the daemon never reads a
// local path and the value stays out of the client transcript.
func (b *mcpBridge) resolveValueFile(msg json.RawMessage) (json.RawMessage, error) {
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(msg, &frame); err != nil {
		return msg, nil
	}
	if frame.Method != "tools/call" || frame.Params.Name != "secret" {
		return msg, nil
	}
	input, ok := frame.Params.Arguments["input"].(map[string]any)
	if !ok {
		return msg, nil
	}
	resolved, err := tools.ResolveValueFile(input)
	if err != nil {
		return nil, err
	}
	if _, had := input["value_file"]; !had {
		return msg, nil
	}
	var full map[string]any
	if err := json.Unmarshal(msg, &full); err != nil {
		return msg, nil
	}
	params, _ := full["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	args["input"] = resolved
	out, err := json.Marshal(full)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// forward posts one message and returns the reply body, empty for a
// notification.
func (b *mcpBridge) forward(ctx context.Context, msg json.RawMessage) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", b.target.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.target.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.session != "" {
		req.Header.Set("Mcp-Session-Id", b.session)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		b.session = id
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	return mcpBodyJSON(resp.Header.Get("Content-Type"), body), nil
}

// mcpBodyJSON unwraps an SSE reply to its JSON payload. A JSON reply passes
// through unchanged.
func mcpBodyJSON(contentType string, body []byte) []byte {
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return bytes.TrimSpace(body)
	}
	var last []byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			last = bytes.TrimSpace(data)
		}
	}
	return last
}
