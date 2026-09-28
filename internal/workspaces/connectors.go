package workspaces

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

// ConnectorOverride is the workspace layer of one connector in trebi.yaml.
// Patch is a JSON merge patch on the resolved manifest.
type ConnectorOverride struct {
	Version  string            `yaml:"version,omitempty" json:"version,omitempty"`
	Track    string            `yaml:"track,omitempty" json:"track,omitempty"`
	Inputs   map[string]string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Patch    map[string]any    `yaml:"patch,omitempty" json:"patch,omitempty"`
	Disabled bool              `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// ConnectorOverrides reads the connectors block of trebi.yaml. A missing
// file or block is an empty map.
func (s *Service) ConnectorOverrides(ctx context.Context, id string) (map[string]ConnectorOverride, error) {
	ws, err := s.getRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	return readConnectorOverrides(ws.Path)
}

func readConnectorOverrides(dir string) (map[string]ConnectorOverride, error) {
	data, err := os.ReadFile(filepath.Join(dir, "trebi.yaml"))
	if os.IsNotExist(err) {
		return map[string]ConnectorOverride{}, nil
	}
	if err != nil {
		return nil, err
	}
	var y trebiYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("trebi.yaml: %w", err)
	}
	if y.Connectors == nil {
		return map[string]ConnectorOverride{}, nil
	}
	return y.Connectors, nil
}

// AllConnectorOverrides reads the connectors block of every live workspace,
// keyed by workspace id. A workspace whose trebi.yaml does not parse is
// skipped.
func (s *Service) AllConnectorOverrides(ctx context.Context) (map[string]map[string]ConnectorOverride, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]ConnectorOverride{}
	for i := range list {
		ws := &list[i]
		if ws.Orphaned {
			continue
		}
		ov, err := readConnectorOverrides(ws.Path)
		if err != nil || len(ov) == 0 {
			continue
		}
		out[ws.ID] = ov
	}
	return out, nil
}

// SetConnectorOverride writes or, with a nil override, removes the workspace
// layer of one connector. It emits workspace.updated.
func (s *Service) SetConnectorOverride(ctx context.Context, id, name string, ov *ConnectorOverride) error {
	ws, err := s.getRaw(ctx, id)
	if err != nil {
		return err
	}
	path := filepath.Join(ws.Path, "trebi.yaml")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var y trebiYAML
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &y); err != nil {
			return fmt.Errorf("trebi.yaml: %w", err)
		}
	}
	y.ID, y.Name, y.GitEnabled = ws.ID, ws.Name, ws.GitEnabled
	if ov == nil {
		delete(y.Connectors, name)
	} else {
		if y.Connectors == nil {
			y.Connectors = map[string]ConnectorOverride{}
		}
		y.Connectors[name] = *ov
	}
	if len(y.Connectors) == 0 {
		y.Connectors = nil
	}
	out, err := yaml.Marshal(&y)
	if err != nil {
		return err
	}
	if err := job.WriteAtomic(path, out); err != nil {
		return err
	}
	_, err = s.st.Append(ctx, store.BusEvent{
		Topic: "workspace.updated", Entity: "workspace", EntityID: id, Action: "updated",
		Attrs: map[string]string{"connector": name},
	})
	return err
}
