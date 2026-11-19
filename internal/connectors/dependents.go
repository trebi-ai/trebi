package connectors

import "context"

// TriggerDependent is one route that names a connector.
type TriggerDependent struct {
	Kind      string   `json:"kind"`      // job | agent | assistant
	Workspace string   `json:"workspace"` // workspace id
	Name      string   `json:"name"`      // index name, as on event outcomes: "<name>" for an agent
	Target    string   `json:"target"`    // target name, as in the routes file
	RouteID   string   `json:"route_id"`  // route id
	Gets      string   `json:"gets"`      // every_message | chosen | by_hand | off
	Active    bool     `json:"active"`
	Broken    string   `json:"broken,omitempty"`
	Rooms     []string `json:"rooms"`
	Senders   int      `json:"senders"` // allowlist size; identities stay out of the list
}

// TriggerDependents lists every route on the connector slug, inactive
// targets, broken routes, and routes that get nothing included.
func (s *Service) TriggerDependents(ctx context.Context, slug string) []TriggerDependent {
	out := []TriggerDependent{}
	if s.ws == nil {
		return out
	}
	for _, t := range s.triggerIndex(ctx)[slug] {
		rooms := t.tr.Rooms
		if rooms == nil {
			rooms = []string{}
		}
		out = append(out, TriggerDependent{
			Kind: t.row.Kind, Workspace: t.row.WorkspaceID, Name: t.row.Name,
			Target: t.route.TargetName(), RouteID: t.tr.Route, Gets: t.route.Gets,
			Active: t.active, Broken: t.broken, Rooms: rooms, Senders: len(t.tr.Allowlist),
		})
	}
	return out
}
