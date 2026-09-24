package cli

import (
	"bytes"
	"context"
	"encoding/base64"
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
					&cli.StringFlag{Name: "client", Value: "json", Usage: "claude, codex, opencode, opencode2, cursor, vscode, or json"},
					&cli.BoolFlag{Name: "command", Usage: "print the add command instead of the config file block"},
				},
				Action: runMCPConfig,
			},
			{
				Name:  "install",
				Usage: "run the add command of one client",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "client", Value: "claude", Usage: "claude, codex, opencode, or opencode2"},
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
	URL string
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
	return mcpTarget{URL: client.base + "/mcp"}, nil
}

// block returns the config file text for one client.
func (t mcpTarget) block(client string) (string, error) {
	switch client {
	case "codex":
		return fmt.Sprintf("[mcp_servers.trebi]\nurl = %q\n", t.URL), nil
	case "claude":
		return t.jsonBlock(map[string]any{"mcpServers": map[string]any{"trebi": map[string]any{
			"type": "http", "url": t.URL,
		}}})
	case "opencode":
		// OpenCode 1.x keys MCP servers by name under "mcp" (McpRemoteConfig),
		// not under a "servers" map. A remote server defaults to OAuth
		// auto-detection; this daemon has none.
		return t.jsonBlock(map[string]any{"mcp": map[string]any{"trebi": map[string]any{
			"type": "remote", "url": t.URL, "oauth": false,
		}}})
	case "opencode2":
		// The 2.x beta writes "mcp.servers.<name>" (its own `mcp add` output)
		// and reads both shapes. Match what it writes.
		return t.jsonBlock(map[string]any{"mcp": map[string]any{"servers": map[string]any{"trebi": map[string]any{
			"type": "remote", "url": t.URL, "oauth": false,
		}}}})
	case "cursor":
		return t.jsonBlock(map[string]any{"mcpServers": map[string]any{"trebi": map[string]any{
			"url": t.URL,
		}}})
	case "vscode":
		return t.jsonBlock(map[string]any{"servers": map[string]any{"trebi": map[string]any{
			"type": "http", "url": t.URL,
		}}})
	case "json", "":
		return t.jsonBlock(map[string]any{"url": t.URL})
	}
	return "", fmt.Errorf("unknown client %q; use claude, codex, opencode, opencode2, cursor, vscode, or json", client)
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
			"--scope", "user"}, nil
	case "codex":
		return []string{"codex", "mcp", "add", "trebi", "--url", t.URL}, nil
	case "opencode", "opencode2":
		// Non-interactive `mcp add` writes the global config
		// (XDG_CONFIG_HOME/opencode/opencode.json); there is no --global flag.
		return []string{client, "mcp", "add", "trebi", "--url", t.URL}, nil
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
	// refresh re-reads api.json. Tests inject a stub; production uses mcpConn.
	refresh func() (mcpTarget, error)
}

func newMCPBridge(target mcpTarget) *mcpBridge {
	return &mcpBridge{target: target, http: &http.Client{}, refresh: mcpConn}
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
		outPath, msg := b.attachmentOut(msg)
		reply, err := b.forward(ctx, msg)
		if err != nil {
			return err
		}
		if outPath != "" && len(reply) > 0 {
			reply, err = b.writeFileReply(reply, outPath)
			if err != nil {
				return err
			}
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

// attachmentOut pulls the client-side `out` field of email.attachment before
// the call reaches the daemon, and returns the path plus the call without
// it. The bridge writes the bytes to that path after the reply, so an
// attachment never rides a transcript and the daemon never sees the path.
func (b *mcpBridge) attachmentOut(msg json.RawMessage) (string, json.RawMessage) {
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Action string         `json:"action"`
				Input  map[string]any `json:"input"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(msg, &frame); err != nil {
		return "", msg
	}
	if frame.Method != "tools/call" || frame.Params.Name != "email" ||
		frame.Params.Arguments.Action != "attachment" {
		return "", msg
	}
	path, _ := frame.Params.Arguments.Input["out"].(string)
	if path == "" {
		return "", msg
	}
	var full map[string]any
	if err := json.Unmarshal(msg, &full); err != nil {
		return "", msg
	}
	params, _ := full["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	input, _ := args["input"].(map[string]any)
	delete(input, "out")
	out, err := json.Marshal(full)
	if err != nil {
		return "", msg
	}
	return path, out
}

// writeFileReply decodes one email.attachment reply, writes the base64 bytes
// to the local path, and replaces the reply with the path. An error reply
// passes through unchanged.
func (b *mcpBridge) writeFileReply(reply json.RawMessage, path string) (json.RawMessage, error) {
	var res struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reply, &res); err != nil || res.Result.IsError || len(res.Result.Content) == 0 {
		return reply, nil
	}
	var payload struct {
		Size          int    `json:"size"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal([]byte(res.Result.Content[0].Text), &payload); err != nil || payload.ContentBase64 == "" {
		return reply, nil
	}
	raw, err := base64.StdEncoding.DecodeString(payload.ContentBase64)
	if err != nil {
		return nil, fmt.Errorf("attachment %s: decode: %w", path, err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return nil, fmt.Errorf("attachment %s: %w", path, err)
	}
	res.Result.Content[0].Text = fmt.Sprintf("wrote %s (%d bytes)", path, len(raw))
	out, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// forward posts one message and returns the reply body, empty for a
// notification. A transport error re-reads api.json once so a daemon
// restart on a new port does not kill the stdio session.
func (b *mcpBridge) forward(ctx context.Context, msg json.RawMessage) ([]byte, error) {
	body, err := b.post(ctx, msg)
	if err == nil || !b.retarget() {
		return body, err
	}
	return b.post(ctx, msg)
}

// retarget reloads the daemon URL from api.json. It reports true only when
// the endpoint actually moved, so a dead daemon is not retried in a loop.
func (b *mcpBridge) retarget() bool {
	if b.refresh == nil {
		return false
	}
	next, err := b.refresh()
	if err != nil || next.URL == "" {
		return false
	}
	if next.URL == b.target.URL {
		return false
	}
	b.target = next
	b.session = ""
	return true
}

func (b *mcpBridge) post(ctx context.Context, msg json.RawMessage) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", b.target.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
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
