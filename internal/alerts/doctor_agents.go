package alerts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
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
	out = append(out, harnessOffFindings(context.Background(), st.DB())...)
	if len(out) == 0 {
		return []DoctorFinding{}
	}
	return out
}

func agentKitFindings(st *store.Store, actions ActionsChecker) []DoctorFinding {
	ctx := context.Background()
	var findings []DoctorFinding
	// The connectors doctor reports each folder that does not load.
	overrides, _ := workspaces.New(st).AllConnectorOverrides(ctx) // best effort; no layers reads as none
	defs, _ := connectors.LoadOffline(overrides)
	db := st.DB()
	rows, err := db.Query(`SELECT id, workspace_id, name, path, kind FROM job_index WHERE retired_at IS NULL`)
	if err != nil {
		return findings
	}
	defer rows.Close()

	sk := skills.New(st).WithOwners(workspaces.New(st))
	findings = append(findings, skillOverwriteFindings(ctx, sk)...)
	inherited := map[string]bool{}
	if userHome, err := os.UserHomeDir(); err == nil {
		for _, server := range harness.DiscoverMCP(userHome, nil) {
			inherited[server.Name] = true
		}
	}

	ops := channelOperators{}
	team := teamRefs{}
	access := triggerAccess{defs: defs, kits: map[string][]string{}}
	for rows.Next() {
		var id, wsID, name, path, kind string
		if err := rows.Scan(&id, &wsID, &name, &path, &kind); err != nil {
			continue
		}
		parsed, err := job.ParseFile(path)
		if err != nil || parsed == nil {
			continue
		}
		short := strings.TrimPrefix(name, "agents/")
		if kind == "agent" {
			ops.add(wsID, name, parsed.Triggers, defs, false)
			team.add(wsID, short, parsed.Team)
		}
		findings = append(findings, triggerConnectorFindings(wsID, short, parsed.Triggers, defs, false)...)
		access.add(wsID, name, kind, parsed)
		findings = append(findings, kitRefFindings(wsID, short, parsed, resolvedNames(ctx, sk, wsID, id, kind))...)
		findings = append(findings, workingDirFindings(wsID, short, kind, parsed)...)
		if actions != nil {
			spec := *parsed
			spec.Name = name
			findings = append(findings, launchFindings(ctx, actions, wsID, short, spec, defs, inherited)...)
		}
	}
	_ = rows.Err()
	findings = append(findings, access.findings()...)
	findings = append(findings, ops.addAssistants(db, defs)...)
	findings = append(findings, team.findings()...)
	return append(findings, ops.findings()...)
}

// triggerConnectorFindings reports each enabled event trigger whose
// connector the workspace cannot use: unknown, turned off, or outside the
// scope of the connection. Such a trigger never fires. An assistant trigger
// with no family is an event trigger.
func triggerConnectorFindings(wsID, short string, triggers []job.Trigger, defs connectors.Offline, assistant bool) []DoctorFinding {
	var out []DoctorFinding
	for _, tr := range triggers {
		event := tr.Family == "event" || (assistant && tr.Family == "")
		if tr.Disabled || !event || tr.Event == "" {
			continue
		}
		id := "trigger-connector-" + wsID + "-" + short + "-" + tr.Event
		switch err := defs.HiddenIn(tr.Event, wsID); {
		case errors.Is(err, connectors.ErrNotFound):
			out = append(out, DoctorFinding{ID: id, Severity: "warn", Code: "trigger_connector_unknown",
				Message: fmt.Sprintf("%s has an event trigger on connector %s, which does not exist; the trigger never fires", short, tr.Event)})
		case errors.Is(err, connectors.ErrDisabled):
			out = append(out, DoctorFinding{ID: id, Severity: "warn", Code: "trigger_connector_disabled",
				Message: fmt.Sprintf("%s has an event trigger on connector %s, which is turned off in this workspace; the trigger never fires", short, tr.Event)})
		case errors.Is(err, connectors.ErrNotInScope):
			out = append(out, DoctorFinding{ID: id, Severity: "warn", Code: "connector_out_of_scope",
				Message: fmt.Sprintf("%s has an event trigger on connector %s, whose scope does not include this workspace; the trigger never fires", short, tr.Event)})
		}
	}
	return out
}

// triggerAccess finds the event triggers whose runs get no action of the
// trigger connection. It needs the kit of each agent, so it reports after
// all rows are read.
type triggerAccess struct {
	defs connectors.Offline
	kits map[string][]string // workspace + "/" + agent name -> kit connectors
	jobs []accessJob
}

type accessJob struct {
	wsID, name string
	job        *job.Job
}

func (t *triggerAccess) add(wsID, name, kind string, parsed *job.Job) {
	if kind == "agent" && parsed.Kit != nil {
		t.kits[wsID+"/"+name] = parsed.Kit.Connectors
	}
	t.jobs = append(t.jobs, accessJob{wsID: wsID, name: name, job: parsed})
}

// findings reports trigger_access_none. The read-only state is normal and
// shows only on the job.
func (t *triggerAccess) findings() []DoctorFinding {
	var out []DoctorFinding
	for _, a := range t.jobs {
		names := slices.Concat(a.job.Connectors, t.kits[a.wsID+"/"+a.name], t.kits[a.wsID+"/"+job.OwnerOf(a.name)])
		for _, tr := range a.job.Triggers {
			if tr.Disabled || tr.Family != "event" || tr.Event == "" || t.defs.HiddenIn(tr.Event, a.wsID) != nil {
				continue
			}
			r, ok := t.defs.Resolve(tr.Event)
			if !ok || r.TriggerAccess(r.Named(names)) != connectors.AccessNone {
				continue
			}
			out = append(out, DoctorFinding{
				ID:       "trigger-access-" + a.wsID + "-" + strings.TrimPrefix(a.name, "agents/") + "-" + tr.Event,
				Severity: "warn", Code: "trigger_access_none",
				Message: fmt.Sprintf("%s has an event trigger on %s. %s is not in the job connectors or the agent kit, and it has no read-only operations, so runs that %s starts get no actions of it.",
					a.name, r.Title, r.Title, r.Title),
			})
		}
	}
	return out
}

// teamRefs maps workspace -> agent short name -> its team.can_ask list.
type teamRefs map[string]map[string][]string

func (t teamRefs) add(wsID, short string, team *job.TeamYAML) {
	if t[wsID] == nil {
		t[wsID] = map[string][]string{}
	}
	if team != nil && team.CanAsk != nil {
		t[wsID][short] = team.CanAsk
	} else {
		t[wsID][short] = nil
	}
}

// findings reports team.can_ask names with no agent in the workspace. It is
// a finding, not a parse error, because an agent can be deleted later.
func (t teamRefs) findings() []DoctorFinding {
	var out []DoctorFinding
	for wsID, agents := range t {
		for short, canAsk := range agents {
			for _, name := range canAsk {
				if _, ok := agents[name]; ok {
					continue
				}
				out = append(out, DoctorFinding{
					ID:       "agent-team-" + wsID + "-" + short + "-" + name,
					Severity: "warn", Code: "agent_team_unknown",
					Message: fmt.Sprintf("agent %s lists %s in team.can_ask, but the workspace has no agent %s", short, name, name),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// channelOperators maps workspace -> channel connector -> operators whose
// event triggers name it. A channel pointer has no operator in its key, so
// two operators on one channel conflict at run time.
type channelOperators map[string]map[string]map[string]bool

// add records the operator of each enabled event trigger on a channel
// connector. An assistant trigger with no family is an event trigger.
func (c channelOperators) add(wsID, operator string, triggers []job.Trigger, defs connectors.Offline, assistant bool) {
	for _, tr := range triggers {
		event := tr.Family == "event" || (assistant && tr.Family == "")
		m, _ := defs.Manifest(tr.Event)
		if tr.Disabled || !event || !slices.Contains(m.Modes(), manifest.ModeReply) {
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

// addAssistants records the event triggers of each workspace assistant and
// returns the findings of their connectors.
func (c channelOperators) addAssistants(db *sql.DB, defs connectors.Offline) []DoctorFinding {
	rows, err := db.Query(`SELECT id, path FROM workspaces WHERE orphaned=0`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DoctorFinding
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
		out = append(out, triggerConnectorFindings(wsID, "assistant", y.Assistant.Triggers, defs, true)...)
	}
	return out
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

// resolvedNames lists the skills that a run of one agent or job uses.
func resolvedNames(ctx context.Context, sk *skills.Service, wsID, id, kind string) map[string]bool {
	rs := skills.RunScope{WorkspaceID: wsID, JobID: id}
	if kind == "agent" {
		rs = skills.RunScope{WorkspaceID: wsID, AgentID: id}
	}
	out := map[string]bool{}
	resolved, err := sk.Resolve(ctx, rs)
	if err != nil {
		return out
	}
	for _, r := range resolved {
		out[r.Name] = true
	}
	return out
}

// skillOverwriteFindings explains each skill that overwrites a wider skill
// of the same name in the runs of its owner.
func skillOverwriteFindings(ctx context.Context, sk *skills.Service) []DoctorFinding {
	list, err := sk.List(ctx, skills.ListOpts{})
	if err != nil {
		return nil
	}
	var out []DoctorFinding
	for _, s := range list {
		if s.Overrides == "" || s.Owner == nil {
			continue
		}
		out = append(out, DoctorFinding{
			ID: "skill-overwrite-" + s.Ref, Severity: "info", Code: "skill_overwrite",
			Message: fmt.Sprintf("the %s skill %s of %s overwrites %s in its runs", s.Scope, s.Name, s.Owner.Name, s.Overrides),
		})
	}
	return out
}

// kitRefFindings reports the kit skills that do not exist, and a
// legacy kit.mcp list.
func kitRefFindings(wsID, short string, parsed *job.Job, skillNames map[string]bool) []DoctorFinding {
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

// workingDirFindings reports a working_dir that is not a folder on this
// computer. Only the value in the file is checked; owned jobs that inherit it
// share the finding of their agent.
func workingDirFindings(wsID, short, kind string, parsed *job.Job) []DoctorFinding {
	if parsed.WorkingDir == "" {
		return nil
	}
	dir, _ := parsed.ResolvedWorkingDir(nil)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	return []DoctorFinding{{
		ID:       "working-dir-" + wsID + "-" + short,
		Severity: "warn", Code: "working_dir_missing",
		Message: fmt.Sprintf("%s %s has working_dir %s, which is not a folder", kind, short, dir),
	}}
}

// connectorFindings checks one connector name of a launch against the
// managed definitions, the inherited servers, and the support table.
// launchFindings checks each connector the launch gives the job.
func launchFindings(ctx context.Context, actions ActionsChecker, wsID, short string, spec job.Job, defs connectors.Offline, inherited map[string]bool) []DoctorFinding {
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

func connectorFindings(ctx context.Context, actions ActionsChecker, wsID, short, name, harnessName, runtime string, defs connectors.Offline, inherited map[string]bool) []DoctorFinding {
	id := "connector-" + wsID + "-" + short + "-" + name
	switch err := defs.HiddenIn(name, wsID); {
	case errors.Is(err, connectors.ErrDisabled):
		return []DoctorFinding{{
			ID: id, Severity: "warn", Code: "kit_connector_disabled",
			Message: fmt.Sprintf("%s references connector %s, which is turned off in this workspace; its runs fail", short, name),
		}}
	case errors.Is(err, connectors.ErrNotInScope):
		return []DoctorFinding{{
			ID: id, Severity: "warn", Code: "connector_out_of_scope",
			Message: fmt.Sprintf("%s references connector %s, whose scope does not include this workspace; its runs fail", short, name),
		}}
	}
	d, ok := defs.Manifest(name)
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

// harnessOffFindings reports each active job or agent on a harness that is off.
func harnessOffFindings(ctx context.Context, db *sql.DB) []DoctorFinding {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT j.workspace_id, COALESCE(w.name, j.workspace_id), j.name, j.path, COALESCE(j.harness,'')
		FROM job_index j LEFT JOIN workspaces w ON w.id = j.workspace_id
		WHERE j.retired_at IS NULL AND j.active=1 ORDER BY j.workspace_id, j.name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DoctorFinding
	for rows.Next() {
		var ws, wsName, name, file, h string
		if rows.Scan(&ws, &wsName, &name, &file, &h) != nil {
			continue
		}
		if h == "" {
			h = cfg.Defaults.Command
		}
		if !harness.Known(h) || cfg.HarnessEnabled(h) {
			continue
		}
		out = append(out, DoctorFinding{
			ID: "harness-off-" + ws + "-" + name, Severity: "warn", Code: "harness_off",
			Message: fmt.Sprintf("%s (workspace %s) uses %s, but %s", titleOf(file, name), wsName, appLabel(h), harness.OffMessage(h)),
		})
	}
	return out
}

// keptMaxAge is the age of a kept version that the doctor reports.
const keptMaxAge = 90 * 24 * time.Hour

// harnessVersionFindings reports a failed last update and a version kept
// longer than keptMaxAge, for each harness that is on.
func (s *Service) harnessVersionFindings(ctx context.Context) []DoctorFinding {
	if s.opts.HarnessVersion == nil {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	var out []DoctorFinding
	for _, name := range harness.Names() {
		if !config.IsFirstClass(name) || !cfg.HarnessEnabled(name) {
			continue
		}
		v, err := s.opts.HarnessVersion(ctx, name)
		if err != nil {
			continue
		}
		if n := len(v.History); n > 0 && !v.History[n-1].OK {
			last := v.History[n-1]
			out = append(out, DoctorFinding{
				ID: "harness-update-failed-" + name, Severity: "warn", Code: "harness_update_failed",
				Message: fmt.Sprintf("%s: the update from %s to %s failed: %s", name, last.From, last.To, last.Error),
			})
		}
		if v.Updates == agentvisor.UpdatesKeep && v.KeptAt != nil && time.Since(*v.KeptAt) > keptMaxAge {
			out = append(out, DoctorFinding{
				ID: "harness-kept-old-" + name, Severity: "info", Code: "harness_kept_old",
				Message: fmt.Sprintf("%s: Trebi keeps version %s since %s. Update it in Settings → Harnesses.", name, v.Version, v.KeptAt.Format("2006-01-02")),
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
