package connectors

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"gopkg.in/yaml.v3"
)

// inspectWait bounds one MCP inspection; inspectTTL keeps its result.
const (
	inspectWait = 20 * time.Second
	inspectTTL  = 10 * time.Minute
)

// Capabilities is what one connector instance offers (Part E.9): the
// manifest blocks, the live session, the skill, and for MCP the live lists.
type Capabilities struct {
	Name         string                     `json:"name"`
	Instance     string                     `json:"instance"`
	Version      string                     `json:"version"`
	Capabilities []string                   `json:"capabilities"`
	Actions      *ActionCapabilities        `json:"actions,omitempty"`
	Events       *manifest.Events           `json:"events,omitempty"`
	Channel      *manifest.Channel          `json:"channel,omitempty"`
	Session      *protocol.InitializeResult `json:"session,omitempty"`
	Skill        *SkillInfo                 `json:"skill,omitempty"`
}

// ActionCapabilities lists the CLI programs and the MCP server of a run.
type ActionCapabilities struct {
	Commands []CommandInfo  `json:"commands"`
	MCP      *MCPInspection `json:"mcp,omitempty"`
}

// CommandInfo is one CLI program and where it is found; Path is "" when it
// is missing.
type CommandInfo struct {
	Command string `json:"command"`
	Path    string `json:"path"`
}

// SkillInfo is the skill of the connector.
type SkillInfo struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Compat      string `json:"compat,omitempty"`
}

// MCPInspection is the live answer of the MCP server. Error is set when the
// server did not start or answer.
type MCPInspection struct {
	Server       *mcp.Implementation `json:"server,omitempty"`
	Instructions string              `json:"instructions,omitempty"`
	Tools        []*mcp.Tool         `json:"tools"`
	Prompts      []*mcp.Prompt       `json:"prompts"`
	Resources    []*mcp.Resource     `json:"resources"`
	InspectedAt  string              `json:"inspected_at"`
	Error        string              `json:"error,omitempty"`
}

type inspected struct {
	at  time.Time
	res *MCPInspection
}

// Capabilities inspects one connector. An instance key wins over the
// workspace.
func (s *Service) Capabilities(ctx context.Context, name, workspaceID, instance string) (Capabilities, error) {
	var r Resolved
	var err error
	if instance != "" {
		if n, _ := splitInstance(instance); n != name {
			return Capabilities{}, fmt.Errorf("connector instance %q not found: %w", instance, ErrNotFound)
		}
		r, err = s.resolveInstance(instance)
	} else {
		r, err = s.Resolve(name, workspaceID)
	}
	if err != nil {
		return Capabilities{}, err
	}
	m := r.Manifest
	out := Capabilities{Name: r.Name, Instance: r.Instance, Version: r.Version, Capabilities: m.Capabilities(), Events: m.Events, Channel: m.Channel}
	if m.Actions != nil {
		ac := &ActionCapabilities{Commands: []CommandInfo{}}
		if m.Actions.CLI != nil {
			path := r.PathEnv(os.Getenv("PATH"))
			for _, c := range m.Actions.CLI.Commands {
				ac.Commands = append(ac.Commands, CommandInfo{Command: c, Path: lookPath(c, path)})
			}
		}
		if m.Actions.MCP != nil {
			ac.MCP = s.inspectMCP(ctx, r)
		}
		out.Actions = ac
	}
	if p := s.procByName(r.Instance); p != nil {
		out.Session = p.snapshot().init
	}
	if dir := r.SkillDir(); dir != "" {
		out.Skill = readSkillInfo(dir)
		if m.Skill != nil {
			out.Skill.Compat = m.Skill.Compat
		}
	}
	return out, nil
}

// readSkillInfo reads the name and description of SKILL.md.
func readSkillInfo(dir string) *SkillInfo {
	info := &SkillInfo{Path: dir, Name: filepath.Base(dir)}
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return info
	}
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return info
	}
	front, _, ok := bytes.Cut(rest, []byte("\n---"))
	if !ok {
		return info
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal(front, &fm) == nil {
		if fm.Name != "" {
			info.Name = fm.Name
		}
		info.Description = fm.Description
	}
	return info
}

// inspectMCP starts the MCP server with the resolved env, reads its lists,
// and closes it. The result is kept for inspectTTL per fingerprint.
func (s *Service) inspectMCP(ctx context.Context, r Resolved) *MCPInspection {
	key := r.fingerprint()
	s.mu.Lock()
	c, ok := s.inspected[key]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < inspectTTL {
		return c.res
	}
	res := s.runInspect(ctx, r)
	s.mu.Lock()
	for k, v := range s.inspected {
		if s.now().Sub(v.at) >= inspectTTL {
			delete(s.inspected, k)
		}
	}
	s.inspected[key] = inspected{at: s.now(), res: res}
	s.mu.Unlock()
	return res
}

func (s *Service) runInspect(ctx context.Context, r Resolved) *MCPInspection {
	out := &MCPInspection{Tools: []*mcp.Tool{}, Prompts: []*mcp.Prompt{}, Resources: []*mcp.Resource{}, InspectedAt: s.now().UTC().Format(time.RFC3339)}
	srv, missing, err := r.MCPServer()
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case len(missing) > 0:
		out.Error = "missing secrets: " + strings.Join(missing, ", ")
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, inspectWait)
	defer cancel()
	var t mcp.Transport
	if srv.Command != "" {
		cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
		cmd.Env = os.Environ()
		for k, v := range srv.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		t = &mcp.CommandTransport{Command: cmd}
	} else {
		t = &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: headerTransport{headers: srv.Headers}}}
	}
	v := s.version
	if v == "" {
		v = "dev"
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "trebi", Version: v}, nil).Connect(ctx, t, nil)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer cs.Close() //nolint:errcheck // the inspection is done
	init := cs.InitializeResult()
	out.Server, out.Instructions = init.ServerInfo, init.Instructions
	var errs []string
	if init.Capabilities != nil && init.Capabilities.Tools != nil {
		for cur, n := "", 0; n < 20; n++ {
			res, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cur})
			if err != nil {
				errs = append(errs, "tools/list: "+err.Error())
				break
			}
			out.Tools = append(out.Tools, res.Tools...)
			if cur = res.NextCursor; cur == "" {
				break
			}
		}
	}
	if init.Capabilities != nil && init.Capabilities.Prompts != nil {
		for cur, n := "", 0; n < 20; n++ {
			res, err := cs.ListPrompts(ctx, &mcp.ListPromptsParams{Cursor: cur})
			if err != nil {
				errs = append(errs, "prompts/list: "+err.Error())
				break
			}
			out.Prompts = append(out.Prompts, res.Prompts...)
			if cur = res.NextCursor; cur == "" {
				break
			}
		}
	}
	if init.Capabilities != nil && init.Capabilities.Resources != nil {
		for cur, n := "", 0; n < 20; n++ {
			res, err := cs.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cur})
			if err != nil {
				errs = append(errs, "resources/list: "+err.Error())
				break
			}
			out.Resources = append(out.Resources, res.Resources...)
			if cur = res.NextCursor; cur == "" {
				break
			}
		}
	}
	out.Error = strings.Join(errs, "; ")
	return out
}

// headerTransport adds the MCP headers of the manifest to each request.
type headerTransport struct{ headers map[string]string }

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}
