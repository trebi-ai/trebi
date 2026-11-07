package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/trebi-ai/trebi/internal/job"
)

// webhookConnector is the trigger connector of a hosted webhook event.
const webhookConnector = "webhook"

// Access is the set of connections that one run can use. The kit gives
// full access. The connection whose event started the run gives read-only
// access when the kit does not name it.
type Access struct {
	Connectors []string // kit: connection ids and slugs
	Trigger    string   // connection id or slug of the read grant; "" when none
}

// InKit reports whether the kit names the connection.
func (a Access) InKit(id, slug string) bool {
	return slices.ContainsFunc(a.Connectors, func(n string) bool { return n == id || n == slug })
}

// Granted reports whether the run reads the connection through the grant.
func (a Access) Granted(id, slug string) bool {
	return a.Trigger != "" && (a.Trigger == id || a.Trigger == slug) && !a.InKit(id, slug)
}

// Allows reports whether the run can call an operation of the connection.
func (a Access) Allows(id, slug string, readOnly bool) bool {
	return a.InKit(id, slug) || (readOnly && a.Granted(id, slug))
}

// grantable reports whether a trigger connector can give a read grant.
// Email and webhook events have no connection.
func grantable(connector string) bool {
	return connector != "" && connector != job.EmailConnector && connector != webhookConnector
}

// RunAccess reads the access of a run from its row. The grant is not
// stored: it comes from the trigger connector on each check.
func (s *Service) RunAccess(ctx context.Context, runID string) Access {
	var conns, trig sql.NullString
	if err := s.st.DB().QueryRowContext(ctx, `SELECT connectors_json, trigger_connector FROM runs WHERE id = ?`, runID).Scan(&conns, &trig); err != nil {
		return Access{}
	}
	var a Access
	if conns.Valid {
		_ = json.Unmarshal([]byte(conns.String), &a.Connectors) // a bad row has no kit
	}
	if grantable(trig.String) {
		a.Trigger = trig.String
	}
	return a
}

// triggerConnector is the trigger connector of a launch. A reopened run
// keeps the trigger of its row, so the grant matches the check.
func (s *Service) triggerConnector(ctx context.Context, opts LaunchOpts) string {
	if opts.ReopenRunID != "" {
		return s.RunAccess(ctx, opts.ReopenRunID).Trigger
	}
	if opts.Trigger != nil && grantable(opts.Trigger.Connector) {
		return opts.Trigger.Connector
	}
	return ""
}

// AccessEntry is one connection that a run can use.
type AccessEntry struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// recordAccess keeps the connections of one launch on the run.
func (s *Service) recordAccess(ctx context.Context, runID string, actions []ConnectorAction) {
	if len(actions) == 0 {
		return
	}
	list := make([]AccessEntry, 0, len(actions))
	for _, a := range actions {
		list = append(list, AccessEntry{ID: a.ID, Slug: a.Slug, Title: a.Title, ReadOnly: a.ReadOnly})
	}
	s.AddEvent(ctx, runID, "access", "runner", map[string]any{"connectors": list})
}

// fillAccess reads the access of the last launch of a run.
func (s *Service) fillAccess(ctx context.Context, r *Run) {
	var raw string
	err := s.st.DB().QueryRowContext(ctx, `SELECT detail_json FROM run_events WHERE run_id = ? AND kind = 'access'
		ORDER BY id DESC LIMIT 1`, r.ID).Scan(&raw)
	if err != nil {
		return
	}
	var d struct {
		Connectors []AccessEntry `json:"connectors"`
	}
	if json.Unmarshal([]byte(raw), &d) == nil {
		r.Access = d.Connectors
	}
}
