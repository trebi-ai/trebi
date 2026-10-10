package config

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDecisionsTimeout = 20 * time.Second
	DefaultArtifactsBytes   = 25 << 20
	DefaultArtifactsKeep    = 30
	DefaultArtifactsSnaps   = 2160 * time.Hour
)

// Decider values of decisions.decider.
var deciderValues = []string{"auto", "harness", "native", "off"} //nolint:gochecknoglobals // static value list, never mutated

var decisionKindRE = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// DecisionsConfig is the decisions block: which model answers the safety
// questions, for example on a reply from a new sender.
type DecisionsConfig struct {
	Decider string                  `yaml:"decider,omitempty"` // auto | harness | native | off
	Timeout string                  `yaml:"timeout,omitempty"`
	Model   string                  `yaml:"model,omitempty"`
	Samples int                     `yaml:"samples,omitempty"`
	Kinds   map[string]DecisionKind `yaml:"kinds,omitempty"`
}

// DecisionKind overrides the decider of one policy.
type DecisionKind struct {
	Decider   string  `yaml:"decider,omitempty"`
	Samples   int     `yaml:"samples,omitempty"`
	HoldBelow float64 `yaml:"hold_below,omitempty"`
}

// TimeoutDuration is the time limit of one decision.
func (d DecisionsConfig) TimeoutDuration() time.Duration {
	return positiveDuration(d.Timeout, DefaultDecisionsTimeout)
}

// ArtifactsConfig is the artifacts block.
type ArtifactsConfig struct {
	MaxBytes      int64  `yaml:"max_bytes,omitempty"`
	KeepVersions  int    `yaml:"keep_versions,omitempty"`
	KeepSnapshots string `yaml:"keep_snapshots,omitempty"`
}

// KeepSnapshotsDuration is how long a snapshot that is not pinned stays.
func (a ArtifactsConfig) KeepSnapshotsDuration() time.Duration {
	return positiveDuration(a.KeepSnapshots, DefaultArtifactsSnaps)
}

// deciderErr checks one decider value; empty is the default.
func deciderErr(key, v string) error {
	if v != "" && !slices.Contains(deciderValues, v) {
		return fmt.Errorf("%s: %q not in [%s]", key, v, strings.Join(deciderValues, ", "))
	}
	return nil
}

// durationErr checks one positive duration value; empty is the default.
func durationErr(key, v string) error {
	if v = strings.TrimSpace(v); v == "" {
		return nil
	}
	if d, err := time.ParseDuration(v); err != nil || d <= 0 {
		return fmt.Errorf("%s: want a positive duration, for example 20s", key)
	}
	return nil
}

func (e *ConfigError) checkDecisions(d DecisionsConfig, a ArtifactsConfig) {
	errs := []error{deciderErr("decisions.decider", d.Decider), durationErr("decisions.timeout", d.Timeout),
		durationErr("artifacts.keep_snapshots", a.KeepSnapshots)}
	if d.Samples < 0 || d.Samples > 9 {
		e.add("decisions.samples", "must be 0 to 9")
	}
	for k, v := range d.Kinds {
		errs = append(errs, deciderErr("decisions.kinds."+k+".decider", v.Decider))
		if v.HoldBelow < 0 || v.HoldBelow > 1 {
			e.add("decisions.kinds."+k+".hold_below", "must be 0 to 1")
		}
	}
	for _, err := range errs {
		if err != nil {
			key, msg, _ := strings.Cut(err.Error(), ": ")
			e.add(key, msg)
		}
	}
	if a.MaxBytes < 0 {
		e.add("artifacts.max_bytes", "must be >= 0")
	}
	if a.KeepVersions < 0 {
		e.add("artifacts.keep_versions", "must be >= 0")
	}
}

// setDecisionsKey handles decisions.* and artifacts.* keys; handled=false
// when path is not one.
func (c *Config) setDecisionsKey(path, value string) (handled bool, err error) {
	v := strings.TrimSpace(value)
	switch path {
	case "decisions.decider":
		if err := deciderErr(path, v); err != nil {
			return true, err
		}
		c.Decisions.Decider = v
		return true, nil
	case "decisions.timeout":
		if err := durationErr(path, v); err != nil {
			return true, err
		}
		c.Decisions.Timeout = v
		return true, nil
	case "artifacts.keep_snapshots":
		if err := durationErr(path, v); err != nil {
			return true, err
		}
		c.Artifacts.KeepSnapshots = v
		return true, nil
	case "decisions.model":
		c.Decisions.Model = v
		return true, nil
	case "decisions.samples", "artifacts.keep_versions", "artifacts.max_bytes":
		n, err := strconv.ParseInt(cmp.Or(v, "0"), 10, 64)
		if err != nil || n < 0 {
			return true, fmt.Errorf("%s: want a number >= 0", path)
		}
		switch path {
		case "decisions.samples":
			c.Decisions.Samples = int(n)
		case "artifacts.keep_versions":
			c.Artifacts.KeepVersions = int(n)
		default:
			c.Artifacts.MaxBytes = n
		}
		return true, nil
	}
	rest, ok := strings.CutPrefix(path, "decisions.kinds.")
	if !ok {
		return false, nil
	}
	return true, c.setDecisionKind(rest, v)
}

// setDecisionKind sets decisions.kinds.<kind>.<field>. The kind name has
// dots, so the field is the last part.
func (c *Config) setDecisionKind(rest, v string) error {
	i := strings.LastIndex(rest, ".")
	if i < 0 || !decisionKindRE.MatchString(rest[:i]) {
		return fmt.Errorf("decisions.kinds.%s: want decisions.kinds.<kind>.<decider|samples|hold_below>", rest)
	}
	kind, field := rest[:i], rest[i+1:]
	k := c.Decisions.Kinds[kind]
	if err := k.set("decisions.kinds."+rest, field, v); err != nil {
		return err
	}
	if c.Decisions.Kinds == nil {
		c.Decisions.Kinds = map[string]DecisionKind{}
	}
	if k == (DecisionKind{}) {
		delete(c.Decisions.Kinds, kind)
	} else {
		c.Decisions.Kinds[kind] = k
	}
	return nil
}

func (k *DecisionKind) set(key, field, v string) error {
	switch field {
	case "decider":
		k.Decider = v
		return deciderErr(key, v)
	case "samples":
		n, err := strconv.Atoi(cmp.Or(v, "0"))
		if err != nil || n < 0 || n > 9 {
			return fmt.Errorf("%s: want 0 to 9", key)
		}
		k.Samples = n
	case "hold_below":
		f, err := strconv.ParseFloat(cmp.Or(v, "0"), 64)
		if err != nil || f < 0 || f > 1 {
			return fmt.Errorf("%s: want 0 to 1", key)
		}
		k.HoldBelow = f
	default:
		return fmt.Errorf("%s: unknown field %s", key, field)
	}
	return nil
}

func (l *pathEntryList) addDecisions(cfg *Config) {
	d, a := cfg.Decisions, cfg.Artifacts
	l.add("decisions.decider", d.Decider, "auto")
	l.add("decisions.timeout", d.TimeoutDuration().String(), DefaultDecisionsTimeout.String())
	l.add("decisions.model", d.Model, "")
	l.add("decisions.samples", strconv.Itoa(max(d.Samples, 1)), "1")
	for _, k := range slices.Sorted(maps.Keys(d.Kinds)) {
		v := d.Kinds[k]
		l.add("decisions.kinds."+k+".decider", v.Decider, "")
		l.add("decisions.kinds."+k+".samples", strconv.Itoa(v.Samples), "")
		l.add("decisions.kinds."+k+".hold_below", strconv.FormatFloat(v.HoldBelow, 'f', -1, 64), "")
	}
	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultArtifactsBytes
	}
	keep := a.KeepVersions
	if keep <= 0 {
		keep = DefaultArtifactsKeep
	}
	l.add("artifacts.max_bytes", strconv.FormatInt(maxBytes, 10), strconv.Itoa(DefaultArtifactsBytes))
	l.add("artifacts.keep_versions", strconv.Itoa(keep), strconv.Itoa(DefaultArtifactsKeep))
	l.add("artifacts.keep_snapshots", a.KeepSnapshotsDuration().String(), DefaultArtifactsSnaps.String())
}
