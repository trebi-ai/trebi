package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/request"
)

// Dispatcher is the seam. api.Server satisfies it via Dispatch.
type Dispatcher interface {
	Dispatch(ctx context.Context, req request.HTTPRequest) (status int, body []byte, err error)
}

// Registry is one catalog plus a dispatcher. MCP and the native loop both
// call through it.
type Registry struct {
	catalog Catalog
	disp    Dispatcher
	native  bool
}

// NewRegistry serves the full catalog. MCP uses this.
func NewRegistry(d Dispatcher) *Registry {
	return &Registry{catalog: NewCatalog(), disp: d}
}

// NewNativeRegistry serves the catalog with the native deny-list:
// system.shutdown (would kill the loop) and config.set of assistant.*
// (the loop editing its own model/key). Everything else is allowed.
func NewNativeRegistry(d Dispatcher) *Registry {
	return &Registry{catalog: NewCatalog(), disp: d, native: true}
}

// Catalog returns the underlying action table.
func (r *Registry) Catalog() Catalog { return r.catalog }

// ToolDef is one LLM/MCP tool: an action enum plus a loose input object.
type ToolDef struct {
	Name        string
	Description string
	Actions     []string
	Destructive bool
	Schema      map[string]any
}

// Tools lists one definition per catalog tool. Native omits denied actions
// from the enum. Measured 2026-09-11: 15 tools serialize to 8871 bytes, well
// under the 20 KiB MCP budget; native adds six workspace tools on top.
func (r *Registry) Tools() []ToolDef {
	names := make([]string, 0, len(r.catalog))
	for tool := range r.catalog {
		names = append(names, tool)
	}
	slices.Sort(names)
	out := make([]ToolDef, 0, len(names))
	for _, tool := range names {
		out = append(out, r.toolDef(tool))
	}
	return out
}

func (r *Registry) toolDef(tool string) ToolDef {
	actions := r.catalog[tool]
	names := make([]string, 0, len(actions)+1)
	destructive := false
	for name, e := range actions {
		if r.denied(tool, name) {
			continue
		}
		names = append(names, name)
		destructive = destructive || e.Destructive
	}
	slices.Sort(names)
	names = append(names, "docs")
	return ToolDef{
		Name:        tool,
		Description: fmt.Sprintf("Trebi %s operations. Actions: %s. Action %q gives the inputs of each action.", tool, strings.Join(names, ", "), "docs"),
		Actions:     names,
		Destructive: destructive,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        names,
					"description": "Operation to run. 'docs' gives the input fields.",
				},
				"input": map[string]any{
					"type":                 "object",
					"description":          "Action inputs: path params, query params, and body fields in one flat object.",
					"additionalProperties": true,
				},
			},
			"required": []string{"action"},
		},
	}
}

// Result is one Call outcome. Status is 0 when dispatch was not reached
// (validation) or when Dispatch returned an error.
type Result struct {
	Status int
	Body   []byte
	Err    error
}

// Call runs one tool action. ctx cancel cuts an in-flight dispatch.
func (r *Registry) Call(ctx context.Context, tool, action string, in Input) Result {
	if in == nil {
		in = Input{}
	}
	if action == "docs" {
		doc, err := r.Docs(tool)
		if err != nil {
			return fail("internal", "no docs for tool "+tool)
		}
		return Result{Status: 200, Body: []byte(doc)}
	}
	entry, ok := r.catalog[tool][action]
	if action == "" || !ok || r.denied(tool, action) {
		msg := fmt.Sprintf("unknown action %q for tool %q; action %q lists the valid ones", action, tool, "docs")
		if owners := r.owners(action); len(owners) > 0 {
			msg += fmt.Sprintf("; %s is on %s", action, strings.Join(owners, ", "))
		}
		return fail("validation", msg)
	}
	if r.native && nativeConfigDenied(tool, action, in) {
		return fail("validation", "assistant.* config is not writable by the native assistant")
	}
	if tool == "secret" && action == "set" {
		resolved, err := ResolveValueFile(in)
		if err != nil {
			return fail("validation", err.Error())
		}
		in = resolved
	}
	built, err := entry.Build(in)
	if err != nil {
		return fail("validation", err.Error())
	}
	if r.disp == nil {
		return fail("internal", "dispatch: no dispatcher")
	}
	status, body, err := r.disp.Dispatch(ctx, built)
	if err != nil {
		return Result{Err: err}
	}
	if status == http.StatusPaymentRequired && tool == "request" {
		res := fail("plan_required", RequestPlanMessage)
		res.Status = status
		return res
	}
	if status < 400 {
		if entry.Transform != nil {
			body, err = entry.Transform(in, status, body)
			if err != nil {
				return fail("internal", "transform: "+err.Error())
			}
		}
		if entry.Bounded {
			body, err = BoundBody(in, body, entry.Tail)
			if err != nil {
				return fail("validation", err.Error())
			}
		}
	}
	return Result{Status: status, Body: body}
}

// Docs is the docs action: signatures plus embedded prose.
func (r *Registry) Docs(tool string) (string, error) {
	if _, ok := r.catalog[tool]; !ok {
		return "", fmt.Errorf("unknown tool %q", tool)
	}
	return Docs(tool)
}

func (r *Registry) denied(tool, action string) bool {
	return r.native && nativeDenied[tool][action]
}

// owners lists the tools that have action, for the unknown-action hint.
func (r *Registry) owners(action string) []string {
	var out []string
	for tool, actions := range r.catalog {
		if _, ok := actions[action]; ok && !r.denied(tool, action) {
			out = append(out, tool)
		}
	}
	slices.Sort(out)
	return out
}

var nativeDenied = map[string]map[string]bool{ //nolint:gochecknoglobals // static deny-list
	"system": {"shutdown": true},
}

func nativeConfigDenied(tool, action string, in Input) bool {
	if tool != "config" || action != "set" {
		return false
	}
	if key, err := in.str("key"); err == nil && strings.HasPrefix(key, "assistant.") {
		return true
	}
	values, err := in.obj("values")
	if err != nil || values == nil {
		return false
	}
	for k := range values {
		if strings.HasPrefix(k, "assistant.") {
			return true
		}
	}
	return false
}

// RequestPlanMessage tells a model what to do when the plan does not
// grant requests. The job must not stop on it.
const RequestPlanMessage = "Requests need Trebi Pro. Do not stop the task for this. Continue without the approval if the job allows it, or end the run and report what needs a decision."

func fail(code, message string) Result {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	status := 400
	if code == "internal" {
		status = 500
	}
	return Result{Status: status, Body: body}
}

type actorKey struct{}

// Actor is the identity marker native attaches so the audit log can
// attribute a catalog call to the run that made it.
type Actor struct {
	Name  string
	RunID string
	Kind  string
	// Job is the index name of the run's job or agent; JobID is its ID.
	Job   string
	JobID string
	// Conversation pins an assistant actor to its session conversation.
	Conversation string
	WorkspaceID  string
	Token        string   // context-local; never persisted or logged
	Scopes       []string // remote MCP grant; context-local, never persisted
}

// ActorRemote is the Kind of a remote MCP client that the cloud forwards.
const ActorRemote = "remote"

// ActorMobile is the Kind of a paired phone, or of another machine of the
// account, on the relay.
const ActorMobile = "mobile"

// WithActor stores the native-loop identity on ctx.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom reads the identity marker, if any.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// HeaderActor and HeaderRunID are the internal headers Dispatch sets from
// ActorFrom so auditMiddleware can log them.
const (
	HeaderActor    = "X-Trebi-Actor"
	HeaderRunID    = "X-Trebi-Run-Id"
	HeaderRunToken = "X-Trebi-Run-Token"
)

// ResolveValueFile replaces a secret value_file field with the file contents
// as value, then drops value_file so it never reaches Dispatch. Shared by
// the native registry and the CLI `trebi mcp serve` bridge.
func ResolveValueFile(in Input) (Input, error) {
	if in == nil {
		return in, nil
	}
	path, _ := in["value_file"].(string)
	if path == "" {
		return in, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("value_file %s: %w", path, err)
	}
	out := Input{}
	for k, v := range in {
		if k == "value_file" {
			continue
		}
		out[k] = v
	}
	out["value"] = strings.TrimRight(string(data), "\n")
	return out, nil
}
