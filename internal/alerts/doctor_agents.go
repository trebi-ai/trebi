package alerts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

// ActionsChecker is the launch view of a job's connector actions.
// *runs.Service satisfies it, so the doctor and the launch path share one
// support table.
type ActionsChecker interface {
	EffectiveActions(ctx context.Context, ws string, j job.Job) (names []string, harness, runtime string, err error)
	ActionsSupported(ctx context.Context, harness, runtime string, hasMCP bool) error
}

// CollectAgentDoctorFindings checks agent-only optional configuration against
// the machine state: installed skills, configured MCP servers, and recent
// writes outside an agent folder. Findings are advisory where plan A.8 says
// the daemon cannot provide a hard OS jail. A nil checker skips the connector
// checks.
func CollectAgentDoctorFindings(st *store.Store, actions ActionsChecker) []DoctorFinding {
	if st == nil {
		return []DoctorFinding{}
	}
	out := make([]DoctorFinding, 0)
	out = append(out, agentKitFindings(st, actions)...)
	out = append(out, agentWriteFindings(context.Background(), st.DB())...)
	if len(out) == 0 {
		return []DoctorFinding{}
	}
	return out
}

func agentKitFindings(st *store.Store, actions ActionsChecker) []DoctorFinding {
	ctx := context.Background()
	var findings []DoctorFinding
	// The connectors doctor reports each folder that does not load.
	defs, _ := connectors.LoadManifests()
	db := st.DB()
	rows, err := db.Query(`SELECT workspace_id, name, path, kind FROM job_index WHERE retired_at IS NULL`)
	if err != nil {
		return findings
	}
	defer rows.Close()

	skillNames := map[string]bool{}
	if listed, err := skills.New(st).List(ctx); err == nil {
		for _, skill := range listed {
			skillNames[skill.Name] = true
		}
	}
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

	ops := channelOperators{}
	for rows.Next() {
		var wsID, name, path, kind string
		if err := rows.Scan(&wsID, &name, &path, &kind); err != nil {
			continue
		}
		parsed, err := job.ParseFile(path)
		if err != nil || parsed == nil {
			continue
		}
		if kind == "agent" {
			ops.add(wsID, name, parsed.Triggers, defs, false)
		}
		short := strings.TrimPrefix(name, "agents/")
		findings = append(findings, kitRefFindings(wsID, short, parsed, skillNames, envsets)...)
		if actions != nil {
			spec := *parsed
			spec.Name = name
			findings = append(findings, launchFindings(ctx, actions, wsID, short, spec, defs, inherited)...)
		}
	}
	_ = rows.Err()
	ops.addAssistants(db, defs)
	return append(findings, ops.findings()...)
}

// channelOperators maps workspace -> channel connector -> operators whose
// event triggers name it. A channel pointer has no operator in its key, so
// two operators on one channel conflict at run time.
type channelOperators map[string]map[string]map[string]bool

// add records the operator of each enabled event trigger on a channel
// connector. An assistant trigger with no family is an event trigger.
func (c channelOperators) add(wsID, operator string, triggers []job.Trigger, defs map[string]manifest.Manifest, assistant bool) {
	for _, tr := range triggers {
		event := tr.Family == "event" || (assistant && tr.Family == "")
		if tr.Disabled || !event || !slices.Contains(defs[tr.Event].Modes(), manifest.ModeReply) {
			continue
		}
		if c[wsID] == nil {
			c[wsID] = map[string]map[string]bool{}
		}
		if c[wsID][tr.Event] == nil {
			c[wsID][tr.Event] = map[string]bool{}
		}
		c[wsID][tr.Event][operator] = true
	}
}

// addAssistants records the event triggers of each workspace assistant.
func (c channelOperators) addAssistants(db *sql.DB, defs map[string]manifest.Manifest) {
	rows, err := db.Query(`SELECT id, path FROM workspaces WHERE orphaned=0`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var wsID, path string
		if err := rows.Scan(&wsID, &path); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, "trebi.yaml"))
		if err != nil {
			continue
		}
		var y struct {
			Assistant *wire.Override `yaml:"assistant"`
		}
		if yaml.Unmarshal(data, &y) != nil || y.Assistant == nil {
			continue
		}
		c.add(wsID, "assistant", y.Assistant.Triggers, defs, true)
	}
}

// findings returns one channel_operator_conflict finding per workspace
// channel that more than one operator names.
func (c channelOperators) findings() []DoctorFinding {
	var out []DoctorFinding
	for _, wsID := range slices.Sorted(maps.Keys(c)) {
		for _, connector := range slices.Sorted(maps.Keys(c[wsID])) {
			names := slices.Sorted(maps.Keys(c[wsID][connector]))
			if len(names) < 2 {
				continue
			}
			out = append(out, DoctorFinding{
				ID:       "channel-operator-conflict-" + wsID + "-" + connector,
				Severity: "warn", Code: "channel_operator_conflict",
				Message: fmt.Sprintf("channel connector %s has triggers for more than one operator (%s); one chat belongs to one operator only",
					connector, strings.Join(names, ", ")),
			})
		}
	}
	return out
}

// kitRefFindings reports the kit skills and envsets that do not exist, and a
// legacy kit.mcp list.
func kitRefFindings(wsID, short string, parsed *job.Job, skillNames, envsets map[string]bool) []DoctorFinding {
	var out []DoctorFinding
	if parsed.Kit != nil {
		for _, skill := range parsed.Kit.Skills {
			if skillNames[skill] {
				continue
			}
			out = append(out, DoctorFinding{
				ID:       "agent-skill-" + wsID + "-" + short + "-" + skill,
				Severity: "warn", Code: "agent_skill_missing",
				Message: fmt.Sprintf("agent %s references missing skill %s", short, skill),
			})
		}
		for _, envset := range parsed.Kit.Envset {
			if envsets[envset] {
				continue
			}
			out = append(out, DoctorFinding{
				ID:       "agent-envset-" + wsID + "-" + short + "-" + envset,
				Severity: "warn", Code: "agent_envset_missing",
				Message: fmt.Sprintf("agent %s references missing envset %s", short, envset),
			})
		}
	}
	if len(parsed.LegacyKitMCP) > 0 {
		out = append(out, DoctorFinding{
			ID:       "kit-mcp-" + wsID + "-" + short,
			Severity: "warn", Code: "kit_mcp_removed",
			Message: fmt.Sprintf("%s has kit.mcp, which trebi ignores; add %s to kit.connectors",
				short, strings.Join(parsed.LegacyKitMCP, ", ")),
		})
	}
	return out
}

// connectorFindings checks one connector name of a launch against the
// managed definitions, the inherited servers, and the support table.
// launchFindings checks each connector the launch gives the job.
func launchFindings(ctx context.Context, actions ActionsChecker, wsID, short string, spec job.Job, defs map[string]manifest.Manifest, inherited map[string]bool) []DoctorFinding {
	names, harnessName, runtime, err := actions.EffectiveActions(ctx, wsID, spec)
	if err != nil {
		return nil
	}
	var out []DoctorFinding
	for _, cn := range names {
		if cn == job.EmailConnector {
			continue
		}
		out = append(out, connectorFindings(ctx, actions, wsID, short, cn, harnessName, runtime, defs, inherited)...)
	}
	return out
}

func connectorFindings(ctx context.Context, actions ActionsChecker, wsID, short, name, harnessName, runtime string, defs map[string]manifest.Manifest, inherited map[string]bool) []DoctorFinding {
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
	if err := actions.ActionsSupported(ctx, harnessName, runtime, d.Actions.MCP != nil); err != nil {
		out = append(out, DoctorFinding{
			ID: id, Severity: "warn", Code: "kit_actions_unsupported",
			Message: fmt.Sprintf("%s runs %s on the %s runtime, which cannot take the MCP actions of connector %s",
				short, harnessName, runtime, name),
		})
	}
	cli := d.Actions.CLI
	if cli == nil {
		return out
	}
	for _, command := range cli.Commands {
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

func agentWriteFindings(ctx context.Context, db *sql.DB) []DoctorFinding {
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := db.Query(`
SELECT r.workspace_id, COALESCE(ji.name,''), r.started_at, COALESCE(r.ended_at,''), w.path, r.working_dir
FROM runs r JOIN workspaces w ON w.id=r.workspace_id LEFT JOIN job_index ji ON ji.id=r.job_id
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
