package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/tools"
)

func toolSpecs(reg *tools.Registry) []llm.Tool {
	obj := func(props map[string]any, req ...string) json.RawMessage {
		m := map[string]any{"type": "object", "properties": props, "required": req}
		b, _ := json.Marshal(m)
		return b
	}
	str := map[string]any{"type": "string"}
	out := []llm.Tool{
		{Name: "bash", Description: "Run a command in the workspace cwd. No login shell.", Parameters: obj(map[string]any{"command": str}, "command")},
		{Name: "read", Description: "Read a workspace file.", Parameters: obj(map[string]any{"path": str}, "path")},
		{Name: "write", Description: "Write a workspace file (create or replace).", Parameters: obj(map[string]any{"path": str, "content": str}, "path", "content")},
		{Name: "edit", Description: "Replace one unique occurrence of old_string in a workspace file.", Parameters: obj(map[string]any{"path": str, "old_string": str, "new_string": str}, "path", "old_string", "new_string")},
		{Name: "glob", Description: "List workspace files matching a glob pattern.", Parameters: obj(map[string]any{"pattern": str}, "pattern")},
		{Name: "grep", Description: "Search workspace files for a regex.", Parameters: obj(map[string]any{"pattern": str, "path": str}, "pattern")},
	}
	if reg == nil {
		return out
	}
	for _, d := range reg.Tools() {
		schema, _ := json.Marshal(d.Schema)
		out = append(out, llm.Tool{Name: d.Name, Description: d.Description, Parameters: schema})
	}
	return out
}

func execTools(ctx context.Context, opts RunOpts, calls []llm.ToolCall) []llm.Message {
	out := make([]llm.Message, 0, len(calls))
	for _, c := range calls {
		if opts.Sink.AddEvent != nil {
			opts.Sink.AddEvent(ctx, "tool", "native", catalogEvent(opts, c))
		}
		if opts.Transcript != nil {
			opts.Transcript.ToolCall(c)
		}
		text := runOne(ctx, opts, c)
		if opts.Transcript != nil {
			opts.Transcript.ToolResult(c.ID, c.Name, text)
		}
		out = append(out, llm.Message{Role: "tool", ToolCallID: c.ID, Name: c.Name, Content: text})
	}
	return out
}

func catalogEvent(opts RunOpts, c llm.ToolCall) map[string]any {
	if opts.Registry != nil {
		if _, ok := opts.Registry.Catalog()[c.Name]; ok {
			var args struct {
				Action string          `json:"action"`
				Input  json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(c.Arguments, &args)
			return map[string]any{"tool": c.Name, "action": args.Action, "input": redactArgs(args.Input)}
		}
	}
	return map[string]any{"name": c.Name, "args": redactArgs(c.Arguments)}
}

func runOne(ctx context.Context, opts RunOpts, c llm.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(c.Arguments, &args)
	str := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	if opts.Registry != nil {
		if _, ok := opts.Registry.Catalog()[c.Name]; ok {
			return runCatalog(ctx, opts, c)
		}
	}
	switch c.Name {
	case "bash":
		return runBash(ctx, opts, str("command"))
	case "read":
		return runRead(ctx, opts, str("path"))
	case "write":
		return runWrite(ctx, opts, str("path"), str("content"))
	case "edit":
		return runEdit(ctx, opts, str("path"), str("old_string"), str("new_string"))
	case "glob":
		return runGlob(opts, str("pattern"))
	case "grep":
		return runGrep(opts, str("pattern"), str("path"))
	default:
		return fmt.Sprintf("unknown tool %q", c.Name)
	}
}

func runCatalog(ctx context.Context, opts RunOpts, c llm.ToolCall) string {
	var args struct {
		Action string      `json:"action"`
		Input  tools.Input `json:"input"`
	}
	if len(c.Arguments) > 0 {
		if err := json.Unmarshal(c.Arguments, &args); err != nil {
			return catalogFail("validation", "decode arguments: "+err.Error())
		}
	}
	ctx = tools.WithActor(ctx, tools.Actor{Name: "assistant", RunID: opts.RunID})
	res := opts.Registry.Call(ctx, c.Name, args.Action, args.Input)
	if res.Err != nil {
		return catalogFail("internal", "dispatch: "+res.Err.Error())
	}
	return string(res.Body)
}

func catalogFail(code, message string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	return string(b)
}

func runBash(ctx context.Context, opts RunOpts, command string) string {
	if strings.TrimSpace(command) == "" {
		return "error: empty command"
	}
	cctx, cancel := context.WithTimeout(ctx, opts.BashTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = opts.WorkingDir
	cmd.Env = opts.Env
	if len(cmd.Env) == 0 {
		cmd.Env = filteredEnv(nil)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		return trunc(fmt.Sprintf("%s\n%v", out, err), 8000)
	}
	return trunc(out, 8000)
}

func runRead(ctx context.Context, opts RunOpts, rel string) string {
	data, err := readJail(ctx, opts, rel)
	if err != nil {
		return "error: " + err.Error()
	}
	return string(data)
}

func runWrite(ctx context.Context, opts RunOpts, rel, content string) string {
	if err := writeJail(ctx, opts, rel, []byte(content)); err != nil {
		return "error: " + err.Error()
	}
	return "wrote " + rel
}

func runEdit(ctx context.Context, opts RunOpts, rel, old, new string) string {
	data, err := readJail(ctx, opts, rel)
	if err != nil {
		return "error: " + err.Error()
	}
	s := string(data)
	n := strings.Count(s, old)
	if n == 0 {
		return "error: old_string not found"
	}
	if n > 1 {
		return fmt.Sprintf("error: old_string matched %d times; must be unique", n)
	}
	if err := writeJail(ctx, opts, rel, []byte(strings.Replace(s, old, new, 1))); err != nil {
		return "error: " + err.Error()
	}
	return "edited " + rel
}

func runGlob(opts RunOpts, pattern string) string {
	if pattern == "" {
		pattern = "**/*"
	}
	root := opts.WorkspaceRoot
	var matches []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		ok, _ := filepath.Match(pattern, rel)
		if !ok {
			ok, _ = filepath.Match(pattern, filepath.Base(rel))
		}
		if ok {
			matches = append(matches, rel)
		}
		if len(matches) >= 200 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "error: " + err.Error()
	}
	if len(matches) == 0 {
		return "(no matches)"
	}
	return strings.Join(matches, "\n")
}

func runGrep(opts RunOpts, pattern, sub string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "error: " + err.Error()
	}
	root := opts.WorkspaceRoot
	if sub != "" {
		joined, jerr := joinRoot(root, sub)
		if jerr != nil {
			return "error: " + jerr.Error()
		}
		root = joined
	}
	var hits []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || len(data) > 1<<20 {
			return nil
		}
		rel, _ := filepath.Rel(opts.WorkspaceRoot, path)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", rel, i+1, trunc(line, 200)))
				if len(hits) >= 100 {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "(no matches)"
	}
	return strings.Join(hits, "\n")
}

var (
	errEscapesRoot = errors.New("path escapes workspace root")
	errTooLarge    = errors.New("file exceeds size limit")
)

func fileCap(opts RunOpts) int64 {
	if opts.MaxFileBytes > 0 {
		return opts.MaxFileBytes
	}
	return 10 << 20
}

func readJail(ctx context.Context, opts RunOpts, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	joined, err := joinRoot(opts.WorkspaceRoot, rel)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(joined)
	if err != nil {
		return nil, err
	}
	if st.Size() > fileCap(opts) {
		return nil, errTooLarge
	}
	return os.ReadFile(joined)
}

func writeJail(ctx context.Context, opts RunOpts, rel string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(data)) > fileCap(opts) {
		return errTooLarge
	}
	joined, err := joinRoot(opts.WorkspaceRoot, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(joined), 0o755); err != nil {
		return err
	}
	return os.WriteFile(joined, data, 0o644)
}

func joinRoot(root, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", errEscapesRoot
	}
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", errEscapesRoot
	}
	root = filepath.Clean(root)
	joined := filepath.Join(root, cleaned)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", errEscapesRoot
	}
	if joined == root {
		return "", errEscapesRoot
	}
	return joined, nil
}

func filteredEnv(extraDeny []string) []string {
	deny := map[string]bool{"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true}
	for _, k := range extraDeny {
		deny[k] = true
	}
	var out []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if deny[k] {
			continue
		}
		out = append(out, e)
	}
	return out
}
