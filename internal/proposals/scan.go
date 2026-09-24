package proposals

import (
	"database/sql"
	"encoding/json"
	"time"
)

const proposalColumns = `id, workspace_id, agent, run_id, rel_path, base_hash,
  base_content, proposed, note, protected_keys, status, decided_by,
  created_at, decided_at, revision`

type rowScanner interface{ Scan(dest ...any) error }

func scanProposal(r rowScanner, p *Proposal, summary bool) error {
	var protected, createdAt string
	var decidedAt sql.NullString
	if err := r.Scan(&p.ID, &p.WorkspaceID, &p.Agent, &p.RunID, &p.RelPath,
		&p.BaseHash, &p.BaseContent, &p.Proposed, &p.Note, &protected,
		&p.Status, &p.DecidedBy, &createdAt, &decidedAt, &p.Revision); err != nil {
		return err
	}
	if summary {
		p.BaseContent, p.Proposed = "", ""
	}
	if err := json.Unmarshal([]byte(protected), &p.ProtectedKeys); err != nil {
		p.ProtectedKeys = []string{}
	}
	var err error
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return err
	}
	if decidedAt.Valid && decidedAt.String != "" {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return err
		}
		p.DecidedAt = &t
	}
	return nil
}
