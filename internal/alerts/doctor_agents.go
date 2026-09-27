package alerts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// CollectAgentDoctorFindings checks agent-only optional configuration against
// the machine state: installed skills, configured MCP servers, and recent
// writes outside an agent folder. Findings are advisory where plan A.8 says
// the daemon cannot provide a hard OS jail.
func CollectAgentDoctorFindings(st *store.Store) []DoctorFinding {
	if st == nil {
		return []DoctorFinding{}
	}
	out := make([]DoctorFinding, 0)
	out = append(out, agentKitFindings(st)...)
	out = append(out, agentWriteFindings(context.Background(), st.DB())...)
	if len(out) == 0 {
		return []DoctorFinding{}
	}
	return out
}

func agentKitFindings(st *store.Store) []DoctorFinding {
	db := st.DB()
	rows, err := db.Query(`SELECT workspace_id, name, path, kind FROM job_index`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	skillNames := map[string]bool{}
	if listed, err := skills.New(st).List(context.Background()); err == nil {
		for _, skill := range listed {
			skillNames[skill.Name] = true
		}
	}
	defs, _ := connectors.LoadDefinitions()
	inherited := map[string]bool{}
	if userHome, err := os.UserHomeDir(); err == nil {
		for _, server := range harness.DiscoverMCP(userHome, nil) {
			inherited[server.Name] = true
		}
	}
	envsets := map[string]bool{}
	if defined, err := config.LoadEnvSets(); err == nil {
		for name := range defined.Sets {
			envsets[name] = true
		}
	}

	var findings []DoctorFinding
	for rows.Next() {
		var wsID, name, path, kind string
		if err := rows.Scan(&wsID, &name, &path, &kind); err != nil {
			continue
		}
		parsed, err := job.ParseFile(path)
		if err != nil || parsed == nil {
			continue
		}
		short := strings.TrimPrefix(name, "agents/")
		if parsed.Kit != nil {
			for _, skill := range parsed.Kit.Skills {
				if skillNames[skill] {
					continue
				}
				findings = append(findings, DoctorFinding{
					ID:       "agent-skill-" + wsID + "-" + short + "-" + skill,
					Severity: "warn", Code: "agent_skill_missing",
					Message: fmt.Sprintf("agent %s references missing skill %s", short, skill),
				})
			}
			for _, envset := range parsed.Kit.Envset {
				if envsets[envset] {
					continue
				}
				findings = append(findings, DoctorFinding{
					ID:       "agent-envset-" + wsID + "-" + short + "-" + envset,
					Severity: "warn", Code: "agent_envset_missing",
					Message: fmt.Sprintf("agent %s references missing envset %s", short, envset),
				})
			}
		}
		for _, name := range connectorNames(parsed, kind) {
			findings = append(findings, connectorFindings(wsID, short, name, parsed, defs, inherited)...)
		}
	}
	_ = rows.Err()
	return findings
}

// connectorNames lists the connector names of one job: the kit list for an
// agent, the job list for everything else.
func connectorNames(parsed *job.Job, kind string) []string {
	if kind == "agent" && parsed.Kit != nil {
		return parsed.Kit.Connectors
	}
	return parsed.Connectors
}

// connectorFindings checks one connector name from a kit or job list against
// the managed definitions, the inherited servers, and the support matrix.
func connectorFindings(wsID, short, name string, parsed *job.Job, defs map[string]connectors.Definition, inherited map[string]bool) []DoctorFinding {
	id := "connector-" + wsID + "-" + short + "-" + name
	d, ok := defs[name]
	if !ok {
		if inherited[name] {
			return []DoctorFinding{{
				ID: id, Severity: "warn", Code: "kit_connector_inherited",
				Message: fmt.Sprintf("%s names inherited MCP server %s; the harness bridge already gives it to the run", short, name),
			}}
		}
		return []DoctorFinding{{
			ID: id, Severity: "warn", Code: "kit_connector_missing",
			Message: fmt.Sprintf("%s references missing connector %s", short, name),
		}}
	}
	if d.Actions == nil {
		return []DoctorFinding{{
			ID: id, Severity: "warn", Code: "kit_connector_no_actions",
			Message: fmt.Sprintf("%s references connector %s, which has no actions block", short, name),
		}}
	}
	var out []DoctorFinding
	if d.Actions.MCP != nil && !actionsSupportedForRun(parsed) {
		out = append(out, DoctorFinding{
			ID: id, Severity: "warn", Code: "kit_actions_unsupported",
			Message: fmt.Sprintf("%s runs %s on the %s runtime, which cannot take the MCP actions of connector %s",
				short, effectiveHarness(parsed), effectiveRuntime(parsed, effectiveHarness(parsed)), name),
		})
	}
	for _, command := range d.Actions.CLI.Commands {
		bin := strings.Fields(command)
		if len(bin) == 0 {
			continue
		}
		if _, err := exec.LookPath(bin[0]); err != nil {
			out = append(out, DoctorFinding{
				ID:       id + "-" + bin[0],
				Severity: "info", Code: "connector_command_missing",
				Message: fmt.Sprintf("connector %s names command %s, which is not on PATH", name, bin[0]),
			})
		}
	}
	return out
}

// effectiveHarness is the harness a job launches under.
func effectiveHarness(parsed *job.Job) string {
	if parsed != nil && parsed.Harness != "" {
		return parsed.Harness
	}
	return "claude"
}

// effectiveRuntime is the runtime a job launches under, the same decision
// wantsCLI makes.
func effectiveRuntime(parsed *job.Job, harness string) string {
	if parsed != nil && parsed.Runtime == "tui" {
		return "tui"
	}
	if strings.EqualFold(os.Getenv("TREBI_HARNESS_RUNTIME"), "tui") {
		return "tui"
	}
	if cfg, err := config.Load(); err == nil && cfg != nil && strings.EqualFold(cfg.HarnessRuntimeFor(harness), "tui") {
		return "tui"
	}
	return "cli"
}

// actionsSupportedForRun is the doctor copy of decision 8: the CLI runtime
// takes MCP servers on the driver harnesses; the pane runtime needs the
// tool's MCPInjector.
func actionsSupportedForRun(parsed *job.Job) bool {
	harnessName := effectiveHarness(parsed)
	tool, ok := tmux.ByName(harnessName)
	if !ok {
		return false
	}
	if effectiveRuntime(parsed, harnessName) == "cli" {
		switch tool.Name() {
		case tmux.Claude, tmux.Codex, tmux.OpenCode, tmux.Copilot, tmux.Cursor, tmux.Gemini:
			return true
		}
		return false
	}
	_, ok = tool.(tmux.MCPInjector)
	return ok
}

func agentWriteFindings(ctx context.Context, db *sql.DB) []DoctorFinding {
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := db.Query(`
SELECT r.workspace_id, r.job, r.started_at, COALESCE(r.ended_at,''), w.path, r.working_dir
FROM runs r JOIN workspaces w ON w.id=r.workspace_id
WHERE r.kind='agent' AND r.ended_at IS NOT NULL AND r.ended_at>=?`, cutoff.Format(time.RFC3339Nano))
	if err != nil {
		return nil
	}
	defer rows.Close()

	seen := map[string]bool{}
	var findings []DoctorFinding
	for rows.Next() {
		var wsID, agent, started, ended, wsPath, workingDir string
		if err := rows.Scan(&wsID, &agent, &started, &ended, &wsPath, &workingDir); err != nil {
			continue
		}
		start, err1 := time.Parse(time.RFC3339Nano, started)
		end, err2 := time.Parse(time.RFC3339Nano, ended)
		if err1 != nil || err2 != nil {
			continue
		}
		root := workingDir
		if root == "" || !filepath.IsAbs(root) {
			root = wsPath
		}
		for _, file := range gitChangedFiles(ctx, root) {
			if insideAgentFolder(file, agent) {
				continue
			}
			abs := filepath.Join(root, file)
			info, err := os.Stat(abs)
			if err != nil || info.ModTime().Before(start) || info.ModTime().After(end.Add(time.Minute)) {
				continue
			}
			key := wsID + "\x00" + file
			if seen[key] {
				continue
			}
			seen[key] = true
			findings = append(findings, DoctorFinding{
				ID:       "agent-write-" + wsID + "-" + fmt.Sprintf("%x", sha256.Sum256([]byte(file)))[:16],
				Severity: "info", Code: "agent_write_outside_folder",
				Message: fmt.Sprintf("agent %s run changed %s outside agents/%s/",
					strings.TrimPrefix(agent, "agents/"), file, strings.TrimPrefix(agent, "agents/")),
			})
		}
	}
	_ = rows.Err()
	return findings
}

func insideAgentFolder(file, agent string) bool {
	clean := filepath.ToSlash(filepath.Clean(file))
	return clean == strings.TrimPrefix(agent, "agents/") ||
		strings.HasPrefix(clean, strings.TrimPrefix(agent, "agents/")+"/")
}

func gitChangedFiles(ctx context.Context, root string) []string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i += 1 {
		field := fields[i]
		if field == "" {
			continue
		}
		// `git status -z` emits "XY PATH" followed by NUL. Rename records emit
		// "XY to\0from"; report the destination, which is the visible path.
		path := field
		if len(path) > 3 {
			path = path[3:]
		} else {
			continue
		}
		path = strings.TrimSpace(path)
		path = strings.Trim(path, `"`)
		if path != "" && !strings.Contains(path, "..") {
			files = append(files, filepath.ToSlash(path))
		}
	}
	return files
}
