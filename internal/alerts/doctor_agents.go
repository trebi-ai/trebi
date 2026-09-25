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
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
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
	rows, err := db.Query(`SELECT workspace_id, name, path FROM job_index WHERE kind='agent'`)
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
	servers := map[string]bool{}
	if userHome, err := os.UserHomeDir(); err == nil {
		for _, server := range harness.DiscoverMCP(userHome, nil) {
			servers[server.Name] = true
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
		var wsID, name, path string
		if err := rows.Scan(&wsID, &name, &path); err != nil {
			continue
		}
		parsed, err := job.ParseFile(path)
		if err != nil || parsed == nil || parsed.Kit == nil {
			continue
		}
		short := strings.TrimPrefix(name, "agents/")
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
		for _, server := range parsed.Kit.MCP {
			if servers[server] {
				continue
			}
			findings = append(findings, DoctorFinding{
				ID:       "agent-mcp-" + wsID + "-" + short + "-" + server,
				Severity: "warn", Code: "agent_mcp_missing",
				Message: fmt.Sprintf("agent %s references missing MCP server %s", short, server),
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
	_ = rows.Err()
	return findings
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
