package assistant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Finding is one doctor finding the assistant contributes.
type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // info|warn
	Code     string `json:"code"`
	Message  string `json:"message"`
	// FixHint names the settings surface that resolves the finding.
	FixHint string `json:"fix_hint,omitempty"`
}

// Doctor reports assistant findings (plan D.1):
//   - no harness resolves for a workspace,
//   - the native harness is selected (or resolved) without a key,
//   - a TREBI.md still exists and the daemon ignores it.
type Doctor struct {
	Res *Resolver
	// Workspaces lists the workspaces to check. The daemon adapts
	// *workspaces.Service.
	Workspaces func(ctx context.Context) ([]WorkspaceInfo, error)
}

// Findings returns the current findings.
func (d *Doctor) Findings(ctx context.Context) []Finding {
	if d == nil || d.Res == nil || d.Workspaces == nil {
		return nil
	}
	infos, err := d.Workspaces(ctx)
	if err != nil {
		return nil
	}
	var out []Finding
	trebiTold := false
	for _, ws := range infos {
		eff, err := d.Res.Effective(ctx, ws.ID)
		if err != nil {
			continue
		}
		if eff.Resolved == "" || !eff.Health.Available {
			out = append(out, Finding{
				ID: "assistant-harness-" + ws.ID, Severity: "warn", Code: "assistant_no_harness",
				Message: fmt.Sprintf("workspace %s: the assistant has no usable harness (%s)",
					ws.Name, strings.TrimSpace(eff.Health.Detail)),
				FixHint: "install a supported CLI or set assistant.native.model and an API key in Settings",
			})
		}
		if eff.Resolved == HarnessNative && eff.Health.Auth == "missing" {
			out = append(out, Finding{
				ID: "assistant-native-key-" + ws.ID, Severity: "warn", Code: "assistant_native_key_missing",
				Message: fmt.Sprintf("workspace %s: the native assistant has no API key", ws.Name),
				FixHint: "set the API key in Settings -> Assistant",
			})
		}
		if !trebiTold {
			if _, err := os.Stat(filepath.Join(ws.Path, "TREBI.md")); err == nil {
				out = append(out, Finding{
					ID: "assistant-trebi-md", Severity: "info", Code: "assistant_trebi_md_ignored",
					Message: "TREBI.md is no longer read. Move its body into Settings -> Assistant -> Context.",
					FixHint: "Settings -> Assistant, or the trebi.yaml assistant block",
				})
				trebiTold = true
			}
		}
	}
	return out
}
