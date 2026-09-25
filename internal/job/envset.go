package job

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvsetList is the envset: frontmatter node (plan H.3). It accepts one set
// name or a list of names. Sets apply in list order; a later set wins on a
// key conflict, and inline env: wins over every set.
type EnvsetList []string

// UnmarshalYAML accepts a scalar name or a sequence of names. A null or
// blank scalar leaves the field unset.
func (e *EnvsetList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!null" || strings.TrimSpace(value.Value) == "" {
			*e = nil
			return nil
		}
		*e = EnvsetList{strings.TrimSpace(value.Value)}
		return nil
	case yaml.SequenceNode:
		names := make(EnvsetList, 0, len(value.Content))
		for _, item := range value.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("envset: want a name or a list of names")
			}
			if name := strings.TrimSpace(item.Value); name != "" {
				names = append(names, name)
			}
		}
		*e = names
		return nil
	default:
		return fmt.Errorf("envset: want a name or a list of names")
	}
}

// MarshalYAML writes one name as a plain scalar, so existing single-set
// files keep their one-line form; two or more names go out as a sequence.
func (e EnvsetList) MarshalYAML() (any, error) {
	switch len(e) {
	case 0:
		return nil, nil
	case 1:
		return e[0], nil
	default:
		return []string(e), nil
	}
}

// List returns the names in apply order. It is nil-safe and never nil, so a
// caller can emit it as a JSON array.
func (e EnvsetList) List() []string {
	out := make([]string, len(e))
	copy(out, e)
	return out
}

// First returns the first name, or "" when unset. It is the legacy
// single-value view used while the payload carries both shapes.
func (e EnvsetList) First() string {
	if len(e) == 0 {
		return ""
	}
	return e[0]
}
