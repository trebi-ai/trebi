package workspaces

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

// ConnectorOverride is the workspace layer of one connection in trebi.yaml,
// keyed by the connection slug. Patch is a JSON merge patch on the actions
// and skill keys of the resolved manifest. A workspace never starts its own
// process: a different account is a second connection.
type ConnectorOverride struct {
	Patch    map[string]any `yaml:"patch,omitempty" json:"patch,omitempty"`
	Disabled bool           `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// overrideKeys are the keys of one layer in trebi.yaml.
var overrideKeys = []string{"patch", "disabled"} //nolint:gochecknoglobals // fixed key list, never mutated

// overridePatchKeys are the manifest keys that a workspace layer can patch.
var overridePatchKeys = []string{"actions", "skill"} //nolint:gochecknoglobals // fixed key list, never mutated

// Validate names the first field that a workspace layer cannot hold.
func (o ConnectorOverride) Validate() error {
	for k := range o.Patch {
		if !slices.Contains(overridePatchKeys, k) {
			return fmt.Errorf("patch.%s: a workspace can patch only %s", k, strings.Join(overridePatchKeys, " and "))
		}
	}
	return nil
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
	if err := checkOverrideKeys(data); err != nil {
		return nil, err
	}
	if y.Connectors == nil {
		return map[string]ConnectorOverride{}, nil
	}
	for slug, ov := range y.Connectors {
		if err := ov.Validate(); err != nil {
			return nil, fmt.Errorf("trebi.yaml: connectors.%s.%w", slug, err)
		}
	}
	return y.Connectors, nil
}

// checkOverrideKeys refuses a key of a connectors layer other than patch and
// disabled, such as inputs or version from the old layer shape.
func checkOverrideKeys(data []byte) error {
	var raw struct {
		Connectors map[string]map[string]yaml.Node `yaml:"connectors"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("trebi.yaml: %w", err)
	}
	for slug, layer := range raw.Connectors {
		for k := range layer {
			if !slices.Contains(overrideKeys, k) {
				return fmt.Errorf("trebi.yaml: connectors.%s.%s: a workspace layer holds only patch and disabled; use a second connection for another account", slug, k)
			}
		}
	}
	return nil
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
		if err := ov.Validate(); err != nil {
			return err
		}
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
