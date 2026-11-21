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
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
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
func CollectAgentDoctorFindings(st *store.Store, actions ActionsChecker, cfg *config.Config) []DoctorFinding {
	if st == nil {
		return []DoctorFinding{}
	}
	out := make([]DoctorFinding, 0)
	out = append(out, agentKitFindings(st, actions)...)
	out = append(out, agentWriteFindings(context.Background(), st.DB())...)
	out = append(out, harnessOffFindings(context.Background(), st.DB(), cfg)...)
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
	rows, err := db.Query(`SELECT id, workspace_id, name, path, kind FROM job_index WHERE retired_at IS NULL AND id NOT IN (` + store.ArchivedJobIDsSQL + `)`)
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

	ops := newChannelOperators()
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
		if kind == "agent" {
			team.add(wsID, name, parsed.Team)
		}
		access.add(wsID, name, kind, parsed)
		findings = append(findings, kitRefFindings(wsID, name, parsed, resolvedNames(ctx, sk, wsID, id, kind, kitSkills(parsed)))...)
		findings = append(findings, workingDirFindings(wsID, name, kind, parsed)...)
		if actions != nil {
			spec := *parsed
			spec.Name = name
			findings = append(findings, launchFindings(ctx, actions, wsID, name, spec, defs, inherited)...)
		}
	}
	_ = rows.Err()
	findings = append(findings, routeFindings(ctx, st, defs, ops, &access)...)
	findings = append(findings, access.findings()...)
	findings = append(findings, team.findings()...)
	return append(findings, ops.findings()...)
}

// routeFindings reads the routes files of each workspace. It records the
// operators and the access of each route, and it reports the connector,
// file, and assistant findings.
func routeFindings(ctx context.Context, st *store.Store, defs connectors.Offline, ops channelOperators, access *triggerAccess) []DoctorFinding {
	var out []DoctorFinding
	wss := workspaces.New(st)
	list, err := wss.List(ctx)
	if err != nil {
		return nil
	}
	for _, ws := range list {
		if ws.Orphaned {
			continue
		}
		out = append(out, assistantTriggerFindings(ws)...)
		files, err := routing.LoadDir(ws.Path)
		if err != nil {
			continue
		}
		targets, err := wss.RouteTargets(ctx, ws.ID)
		if err != nil {
			continue
		}
		for _, f := range files {
			out = append(out, routeFileFindings(ws, f, targets, defs, ops, access)...)
		}
	}
	return out
}

// assistantTriggerFindings reports a triggers key in the assistant block.
// Trebi does not run it.
func assistantTriggerFindings(ws workspaces.Workspace) []DoctorFinding {
	data, err := os.ReadFile(filepath.Join(ws.Path, "trebi.yaml"))
	if err != nil || !workspaces.AssistantHasTriggers(data) {
		return nil
	}
	return []DoctorFinding{{
		ID: "assistant-triggers-" + ws.ID, Severity: "error", Code: "assistant_triggers_removed",
		Message: "trebi.yaml assistant.triggers is not run. Event triggers are routes now. Move each trigger to routes/<connection>.yaml with `assistant: true`, then remove the key.",
	}}
}

// routeFileFindings reports the problems of one routes file and the
// connector findings of its routes.
func routeFileFindings(ws workspaces.Workspace, f routing.File, targets map[string]routing.Target, defs connectors.Offline, ops channelOperators, access *triggerAccess) []DoctorFinding {
	var out []DoctorFinding
	wsID := ws.ID
	file := "routes/" + f.Connection + ".yaml"
	problems := slices.Concat(f.Problems, routing.Check(f, targets))
	for _, p := range problems {
		if p.Code == job.CodeReplyRoomTaken { // reply_room_conflict owns it
			continue
		}
		severity := p.Severity
		if severity == job.SeverityWarning {
			severity = "warn"
		}
		msg := fmt.Sprintf("%s: %s", file, p.Message)
		if p.Fix != nil {
			msg += ". Run: " + p.Fix.CLI(ws.Name)
		}
		out = append(out, DoctorFinding{
			ID: "route-" + wsID + "-" + f.Connection + "-" + p.Code + "-" + p.Path, Severity: severity, Code: p.Code,
			Message: msg,
		})
	}
	if errors.Is(defs.HiddenIn(f.Connection, wsID), connectors.ErrNotFound) {
		out = append(out, DoctorFinding{
			ID: "route-connection-" + wsID + "-" + f.Connection, Severity: "warn", Code: "route_connection_unknown",
			Message: fmt.Sprintf("%s has no connection %s; its routes never fire", file, f.Connection),
		})
	}
	for _, r := range f.Routes {
		if r.Broken() || r.Gets == routing.GetsOff {
			continue
		}
		tr := r.Trigger(f.Connection)
		operator := r.TargetName()
		switch {
		case r.Assistant:
			ops.add(wsID, operator, tr, defs)
		case r.Job != "":
			ops.addReplyJob(wsID, job.OwnerOf(operator), operator, tr, defs)
		default:
			ops.add(wsID, operator, tr, defs)
		}
		access.addRoute(wsID, operator, tr)
		out = append(out, triggerConnectorFindings(wsID, r.TargetName(), r.ID, tr, defs)...)
	}
	return out
}

// triggerConnectorFindings reports an event trigger whose connector the
// workspace cannot use: unknown, turned off, or outside the scope of the
// connection. Such a route never fires.
func triggerConnectorFindings(wsID, target, routeID string, tr job.Trigger, defs connectors.Offline) []DoctorFinding {
	id := "trigger-connector-" + wsID + "-" + target + "-" + tr.Event + "-" + routeID
	switch err := defs.HiddenIn(tr.Event, wsID); {
	case errors.Is(err, connectors.ErrNotFound):
		return []DoctorFinding{{ID: id, Severity: "warn", Code: "trigger_connector_unknown",
			Message: fmt.Sprintf("route %s of %s is on connector %s, which does not exist; the route never fires", routeID, target, tr.Event)}}
	case errors.Is(err, connectors.ErrDisabled):
		return []DoctorFinding{{ID: id, Severity: "warn", Code: "trigger_connector_disabled",
			Message: fmt.Sprintf("route %s of %s is on connector %s, which is turned off in this workspace; the route never fires", routeID, target, tr.Event)}}
	case errors.Is(err, connectors.ErrNotInScope):
		return []DoctorFinding{{ID: id, Severity: "warn", Code: "connector_out_of_scope",
			Message: fmt.Sprintf("route %s of %s is on connector %s, whose scope does not include this workspace; the route never fires", routeID, target, tr.Event)}}
	}
	return nil
}

// triggerAccess finds the routes whose runs get no action of the trigger
// connection. It needs the kit of each agent, so it reports after all rows
// are read.
type triggerAccess struct {
	defs   connectors.Offline
	kits   map[string][]string // workspace + "/" + agent name -> kit connectors
	jobs   map[string]*job.Job // workspace + "/" + index name -> job
	routes []accessRoute
}

type accessRoute struct {
	wsID, name string
	tr         job.Trigger
}

func (t *triggerAccess) add(wsID, name, kind string, parsed *job.Job) {
	if kind == "agent" && parsed.Kit != nil {
		t.kits[wsID+"/"+name] = parsed.Kit.Connectors
	}
	if t.jobs == nil {
		t.jobs = map[string]*job.Job{}
	}
	t.jobs[wsID+"/"+name] = parsed
}

func (t *triggerAccess) addRoute(wsID, name string, tr job.Trigger) {
	t.routes = append(t.routes, accessRoute{wsID: wsID, name: name, tr: tr})
}

// findings reports trigger_access_none. The read-only state is normal and
// shows only on the job.
func (t *triggerAccess) findings() []DoctorFinding {
	var out []DoctorFinding
	for _, a := range t.routes {
		j := t.jobs[a.wsID+"/"+a.name]
		if j == nil || t.defs.HiddenIn(a.tr.Event, a.wsID) != nil {
			continue
		}
		names := slices.Concat(j.Connectors, t.kits[a.wsID+"/"+a.name], t.kits[a.wsID+"/"+job.OwnerOf(a.name)])
		r, ok := t.defs.Resolve(a.tr.Event)
		if !ok || r.TriggerAccess(r.Named(names)) != connectors.AccessNone {
			continue
		}
		out = append(out, DoctorFinding{
			ID:       "trigger-access-" + a.wsID + "-" + a.name + "-" + a.tr.Event + "-" + a.tr.Route,
			Severity: "warn", Code: "trigger_access_none",
			Message: fmt.Sprintf("%s has route %s on %s. %s is not in the job connectors or the agent kit, and it has no read-only operations, so runs that %s starts get no actions of it.",
				a.name, a.tr.Route, r.Title, r.Title, r.Title),
		})
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

// operatorScope is the rooms of one operator trigger; empty is all rooms.
type operatorScope struct {
	operator string
	rooms    []string
}

// channelOperators holds, per workspace and channel connector, the scopes
// of the operators whose event triggers name it, and the reply jobs of each
// room. A channel pointer has no operator in its key, so two operators on
// one room conflict at run time.
type channelOperators struct {
	scopes  map[string]map[string][]operatorScope     // ws → connector → scopes
	replies map[string]map[string]map[string][]string // ws → connector → room → reply jobs
}

func newChannelOperators() channelOperators {
	return channelOperators{scopes: map[string]map[string][]operatorScope{}, replies: map[string]map[string]map[string][]string{}}
}

// channelTrigger reports whether tr is on a channel connector.
func channelTrigger(tr job.Trigger, defs connectors.Offline) bool {
	m, _ := defs.Manifest(tr.Event)
	return slices.Contains(m.Modes(), manifest.ModeReply)
}

func (c channelOperators) addScope(wsID, connector string, s operatorScope) {
	if c.scopes[wsID] == nil {
		c.scopes[wsID] = map[string][]operatorScope{}
	}
	c.scopes[wsID][connector] = append(c.scopes[wsID][connector], s)
}

// add records the scope of an enabled channel route of an operator.
func (c channelOperators) add(wsID, operator string, tr job.Trigger, defs connectors.Offline) {
	if channelTrigger(tr, defs) {
		c.addScope(wsID, tr.Event, operatorScope{operator: operator, rooms: tr.Rooms})
	}
}

// addReplyJob records a chat: reply route of a job under its owner agent,
// so a reply job and its owner never conflict. It also records the job per
// room for the duplicate check.
func (c channelOperators) addReplyJob(wsID, owner, jobName string, tr job.Trigger, defs connectors.Offline) {
	if !channelTrigger(tr, defs) || !tr.RepliesInChat() {
		return
	}
	c.addScope(wsID, tr.Event, operatorScope{operator: owner, rooms: tr.Rooms})
	if c.replies[wsID] == nil {
		c.replies[wsID] = map[string]map[string][]string{}
	}
	if c.replies[wsID][tr.Event] == nil {
		c.replies[wsID][tr.Event] = map[string][]string{}
	}
	for _, room := range tr.Rooms {
		c.replies[wsID][tr.Event][room] = append(c.replies[wsID][tr.Event][room], jobName)
	}
}

// roomsOverlap reports if two room scopes share a room. An empty scope is
// all rooms.
func roomsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	return slices.ContainsFunc(a, func(r string) bool { return slices.Contains(b, r) })
}

// sharedRooms names the rooms of two overlapping scopes.
func sharedRooms(a, b []string) []string {
	switch {
	case len(a) == 0 && len(b) == 0:
		return []string{"all chats"}
	case len(a) == 0:
		return b
	case len(b) == 0:
		return a
	}
	var out []string
	for _, r := range a {
		if slices.Contains(b, r) {
			out = append(out, r)
		}
	}
	return out
}

// findings returns one channel_operator_conflict finding per workspace
// channel where two operators share a room, and one reply_room_conflict
// finding per room that more than one reply job claims.
func (c channelOperators) findings() []DoctorFinding {
	var out []DoctorFinding
	for _, wsID := range slices.Sorted(maps.Keys(c.scopes)) {
		for _, connector := range slices.Sorted(maps.Keys(c.scopes[wsID])) {
			scopes := c.scopes[wsID][connector]
			names, rooms := map[string]bool{}, map[string]bool{}
			for i, a := range scopes {
				for _, b := range scopes[i+1:] {
					if a.operator == b.operator || !roomsOverlap(a.rooms, b.rooms) {
						continue
					}
					names[a.operator], names[b.operator] = true, true
					for _, r := range sharedRooms(a.rooms, b.rooms) {
						rooms[r] = true
					}
				}
			}
			if len(names) == 0 {
				continue
			}
			out = append(out, DoctorFinding{
				ID:       "channel-operator-conflict-" + wsID + "-" + connector,
				Severity: "warn", Code: "channel_operator_conflict",
				Message: fmt.Sprintf("channel connector %s has triggers for more than one operator (%s) in %s; one chat belongs to one operator only",
					connector, strings.Join(slices.Sorted(maps.Keys(names)), ", "), strings.Join(slices.Sorted(maps.Keys(rooms)), ", ")),
			})
		}
	}
	for _, wsID := range slices.Sorted(maps.Keys(c.replies)) {
		for _, connector := range slices.Sorted(maps.Keys(c.replies[wsID])) {
			for _, room := range slices.Sorted(maps.Keys(c.replies[wsID][connector])) {
				jobs := slices.Sorted(slices.Values(c.replies[wsID][connector][room]))
				if len(jobs) < 2 {
					continue
				}
				out = append(out, DoctorFinding{
					ID:       "reply-room-conflict-" + wsID + "-" + connector + "-" + room,
					Severity: "warn", Code: "reply_room_conflict",
					Message: fmt.Sprintf("room %s of connector %s has more than one job that replies (%s); %s replies, the others only listen",
						room, connector, strings.Join(jobs, ", "), jobs[0]),
				})
			}
		}
	}
	return out
}

func kitSkills(parsed *job.Job) []string {
	if parsed.Kit == nil {
		return nil
	}
	return parsed.Kit.Skills
}

// resolvedNames lists the skills that a run of one agent or job uses. An
// archived skill that the kit names counts as present: the archive is not a
// fault.
func resolvedNames(ctx context.Context, sk *skills.Service, wsID, id, kind string, kit []string) map[string]bool {
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
	archived, _ := sk.ArchivedFor(ctx, rs, kit)
	for _, a := range archived {
		out[a.Name] = true
	}
	return out
}

// skillOverwriteFindings explains each skill that overwrites a wider skill
// of the same name in the runs of its owner.
func skillOverwriteFindings(ctx context.Context, sk *skills.Service) []DoctorFinding {
	list, err := sk.List(ctx, skills.ListOpts{}) // archived skills stay out
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

// kitRefFindings reports the kit skills that do not exist.
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
func harnessOffFindings(ctx context.Context, db *sql.DB, cfg *config.Config) []DoctorFinding {
	if cfg == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT j.workspace_id, COALESCE(w.name, j.workspace_id), j.name, j.path, COALESCE(j.harness,'')
		FROM job_index j LEFT JOIN workspaces w ON w.id = j.workspace_id
		WHERE j.retired_at IS NULL AND j.active=1 AND j.id NOT IN (`+store.ArchivedJobIDsSQL+`) ORDER BY j.workspace_id, j.name`)
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
func (s *Service) harnessVersionFindings(ctx context.Context, cfg *config.Config) []DoctorFinding {
	if s.opts.HarnessVersion == nil || cfg == nil {
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
				Message: fmt.Sprintf("agent %s run changed %s outside %s/", agent, file, job.Dir(agent)),
			})
		}
	}
	_ = rows.Err()
	return findings
}

func insideAgentFolder(file, agent string) bool {
	clean := filepath.ToSlash(filepath.Clean(file))
	return clean == agent || strings.HasPrefix(clean, agent+"/")
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
