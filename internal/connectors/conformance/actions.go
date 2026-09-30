package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

// Timeouts of the action checks.
const (
	helpWait = 10 * time.Second
	mcpWait  = 20 * time.Second
	mcpPages = 20
)

// placeholderRE is a ${NAME} input reference in an MCP field.
var placeholderRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// actionsCLISuite checks each CLI command: it is on the version PATH, and
// `<cmd> --help` exits 0 with the clean env and no inputs.
type actionsCLISuite struct{}

func (actionsCLISuite) name() string { return SuiteActionsCLI }

func (actionsCLISuite) applies(m manifest.Manifest) bool {
	return m.Actions != nil && m.Actions.CLI != nil
}

func (actionsCLISuite) run(ctx context.Context, r *run) []Result {
	var out []Result
	for _, c := range r.t.Manifest.Actions.CLI.Commands {
		path := r.lookPath(c)
		if path == "" {
			out = append(out,
				row(SuiteActionsCLI, "path "+c, fmt.Errorf("the command %q is not on the version PATH (<dir>/bin, then PATH)", c)),
				Result{Suite: SuiteActionsCLI, Check: "help " + c, Status: StatusSkip, Message: "not on the PATH"})
			continue
		}
		out = append(out, row(SuiteActionsCLI, "path "+c, nil), row(SuiteActionsCLI, "help "+c, r.help(ctx, path)))
	}
	return out
}

// help runs `<path> --help` with the clean env and no inputs.
func (r *run) help(ctx context.Context, path string) error {
	d, err := r.folders()
	if err != nil {
		return err
	}
	hctx, cancel := context.WithTimeout(ctx, helpWait)
	defer cancel()
	cmd := exec.CommandContext(hctx, path, "--help")
	cmd.Env = r.env(d, nil)
	cmd.Dir = r.t.Dir
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	switch {
	case hctx.Err() != nil:
		return fmt.Errorf("--help did not exit in %s", helpWait)
	case err != nil:
		return fmt.Errorf("--help: %w: %s", err, lastLine(out))
	}
	return nil
}

// lastLine is the last non-empty line of a program output.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// actionsMCPSuite starts the MCP server and checks initialize and
// tools/list.
type actionsMCPSuite struct{}

func (actionsMCPSuite) name() string { return SuiteActionsMCP }

func (actionsMCPSuite) applies(m manifest.Manifest) bool {
	return m.Actions != nil && m.Actions.MCP != nil
}

func (actionsMCPSuite) run(ctx context.Context, r *run) []Result {
	m := r.t.Manifest.Actions.MCP
	if m.Command == "" && m.URL == "" {
		return []Result{{Suite: SuiteActionsMCP, Check: "initialize", Status: StatusSkip,
			Message: "a registry reference (" + m.Name + "); an install resolves it"}}
	}
	cctx, cancel := context.WithTimeout(ctx, mcpWait)
	defer cancel()
	cs, err := r.connectMCP(cctx, m)
	if err != nil {
		return []Result{row(SuiteActionsMCP, "initialize", fmt.Errorf("%w (limit %s)", err, mcpWait))}
	}
	defer cs.Close() //nolint:errcheck // the check is done
	out := []Result{row(SuiteActionsMCP, "initialize", nil)}
	init := cs.InitializeResult()
	if init.Capabilities == nil || init.Capabilities.Tools == nil {
		return append(out, row(SuiteActionsMCP, "tools/list", errors.New("the server does not declare the tools capability")))
	}
	n := 0
	for cur, page := "", 0; page < mcpPages; page++ {
		res, err := cs.ListTools(cctx, &mcp.ListToolsParams{Cursor: cur})
		if err != nil {
			return append(out, row(SuiteActionsMCP, "tools/list", err))
		}
		n += len(res.Tools)
		if cur = res.NextCursor; cur == "" {
			break
		}
	}
	if n == 0 {
		return append(out, row(SuiteActionsMCP, "tools/list", errors.New("the server lists no tool")))
	}
	return append(out, Result{Suite: SuiteActionsMCP, Check: "tools/list", Status: StatusPass, Message: fmt.Sprintf("%d tools", n)})
}

// connectMCP starts or dials the MCP server. ${NAME} in args, env, url,
// and headers expands from the inputs, and every input is also in the
// server env.
func (r *run) connectMCP(ctx context.Context, m *manifest.MCPAction) (*mcp.ClientSession, error) {
	vals := r.inputs()
	expand := func(s string) string {
		return placeholderRE.ReplaceAllStringFunc(s, func(ph string) string { return vals[ph[2:len(ph)-1]] })
	}
	var t mcp.Transport
	if m.Command != "" {
		d, err := r.folders()
		if err != nil {
			return nil, err
		}
		path := r.lookPath(m.Command)
		if path == "" {
			return nil, fmt.Errorf("the command %q is not on the version PATH", m.Command)
		}
		args := make([]string, 0, len(m.Args))
		for _, a := range m.Args {
			args = append(args, expand(a))
		}
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Dir = r.t.Dir
		cmd.Env = r.env(d, vals)
		for k, v := range m.Env {
			cmd.Env = append(cmd.Env, k+"="+expand(v))
		}
		t = &mcp.CommandTransport{Command: cmd}
	} else {
		headers := map[string]string{}
		for k, v := range m.Headers {
			headers[k] = expand(v)
		}
		t = &mcp.StreamableClientTransport{Endpoint: expand(m.URL), HTTPClient: &http.Client{Transport: headerTransport(headers)}}
	}
	return mcp.NewClient(&mcp.Implementation{Name: "trebi-conformance", Version: "1"}, nil).Connect(ctx, t, nil)
}

// headerTransport adds the MCP headers of the manifest to each request.
type headerTransport map[string]string

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}
