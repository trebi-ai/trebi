package connectors

import "context"

// TriggerDependent is one event trigger that names a connector.
type TriggerDependent struct {
	Kind      string   `json:"kind"` // job | agent | assistant
	Workspace string   `json:"workspace"`
	Name      string   `json:"name"`
	OnIndex   int      `json:"on_index"`
	Disabled  bool     `json:"disabled"`
	Active    bool     `json:"active"`
	Rooms     []string `json:"rooms"`
	Senders   int      `json:"senders"` // allowlist size; identities stay out of the list
}

// TriggerDependents lists every event trigger on the connector slug,
// inactive jobs and disabled triggers included.
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
			Kind: t.row.Kind, Workspace: t.row.WorkspaceID, Name: t.j.Name,
			OnIndex: t.tr.Index, Disabled: t.disabled, Active: t.active,
			Rooms: rooms, Senders: len(t.tr.Allowlist),
		})
	}
	return out
}
