package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EnvSetsFile is ~/.trebi/envsets.yaml: set name → env map.
type EnvSetsFile struct {
	Sets map[string]map[string]string
}

// LoadEnvSets reads envsets.yaml (missing → empty); expands ${ENV} at load.
func LoadEnvSets() (*EnvSetsFile, error) {
	ef := &EnvSetsFile{Sets: map[string]map[string]string{}}

	data, err := os.ReadFile(envSetsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return ef, nil
		}
		return nil, err
	}

	raw := map[string]map[string]string{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing envsets.yaml: %w", err)
	}

	for name, kv := range raw {
		expanded := make(map[string]string, len(kv))
		for k, v := range kv {
			expanded[k] = os.ExpandEnv(v)
		}
		ef.Sets[name] = expanded
	}

	return ef, nil
}

func envSetsPath() string {
	return filepath.Join(BaseDir(), "envsets.yaml")
}

// Get returns the named env set; the bool is false when undefined.
func (ef *EnvSetsFile) Get(name string) (map[string]string, bool) {
	if ef == nil {
		return nil, false
	}
	kv, ok := ef.Sets[name]
	return kv, ok
}
