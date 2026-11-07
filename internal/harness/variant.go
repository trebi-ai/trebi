package harness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

// Sign-in methods of a harness based on Claude Code. They are a view of the
// auth mode and the env: they are not stored.
const (
	SignInExisting = "existing" // the login of the user on this machine
	SignInOwn      = "own"      // a login of its own (auth mode separate)
	SignInToken    = "token"    // a login token in CLAUDE_CODE_OAUTH_TOKEN
	SignInService  = "service"  // another service in ANTHROPIC_BASE_URL
)

// ErrVariantExists refuses a new variant with the name of a harness.
var ErrVariantExists = errors.New("a harness with this name exists")

// ErrInvalid is a bad name, base, or env key.
var ErrInvalid = errors.New("invalid")

// ErrNotVariant refuses to delete a built-in harness.
var ErrNotVariant = errors.New("only a harness that you made can be deleted")

// EnvValue is one env value of a harness. The API never returns the value
// of a secret: Set tells if it has one.
type EnvValue struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret"`
	Set    bool   `json:"set,omitempty"`
}

// EnvChange is one key of an env change. A nil change removes the key. A
// change with Secret and no Value makes a plain value a secret.
type EnvChange struct {
	Value  *string `json:"value,omitempty"`
	Secret *bool   `json:"secret,omitempty"`
}

// Variant is a new harness that runs the program of Base with its own home,
// login, and env.
type Variant struct {
	Name     string                `json:"name"`
	Base     string                `json:"base"`
	Label    string                `json:"label,omitempty"`
	AuthMode string                `json:"auth_mode,omitempty"`
	Model    string                `json:"model,omitempty"`
	Effort   string                `json:"effort,omitempty"`
	Env      map[string]*EnvChange `json:"env,omitempty"`
}

// Env lists the env of harness name in key order. A value that names the
// secret of this harness is a secret, and its value stays out.
func Env(cfg *config.Config, name string) []EnvValue {
	p := cfg.Tools.Pin(name)
	if p == nil {
		return []EnvValue{}
	}
	out := make([]EnvValue, 0, len(p.Env))
	for _, k := range slices.Sorted(maps.Keys(p.Env)) {
		v := p.Env[k]
		if v == "secret:"+secretName(name, k) {
			out = append(out, EnvValue{Key: k, Secret: true, Set: secrets.HomeDir().Has(secretName(name, k))})
			continue
		}
		out = append(out, EnvValue{Key: k, Value: v})
	}
	return out
}

// SignIn is the sign-in method of a harness based on Claude Code, else "".
func SignIn(cfg *config.Config, name string) string {
	if cfg.HarnessBase(name) != "claude" {
		return ""
	}
	env := map[string]string{}
	if p := cfg.Tools.Pin(name); p != nil {
		env = p.Env
	}
	switch {
	case env["ANTHROPIC_BASE_URL"] != "":
		return SignInService
	case env["CLAUDE_CODE_OAUTH_TOKEN"] != "":
		return SignInToken
	case cfg.Tools.AuthMode(name) == AuthModeSeparate:
		return SignInOwn
	}
	return SignInExisting
}

// secretName is the secret that holds the env value key of harness name.
func secretName(name, key string) string { return "harness." + name + "." + key }

// checkEnvKey refuses a key that is not portable, or one that Trebi sets.
func checkEnvKey(key string) error {
	if !config.ValidEnvKey(key) {
		return fmt.Errorf("%w: %q is not a valid env name", ErrInvalid, key)
	}
	if key == "CLAUDE_CONFIG_DIR" || key == "CLAUDE_SECURESTORAGE_CONFIG_DIR" || strings.HasPrefix(key, "TREBI_") {
		return fmt.Errorf("%w: Trebi sets %s", ErrInvalid, key)
	}
	return nil
}

// applyEnv changes the env of pin. It writes the secret values and returns
// the secrets to remove after the config is saved.
func applyEnv(name string, pin *config.ToolPin, changes map[string]*EnvChange) (drop []string, err error) { //nolint:gocyclo // one case per kind of change
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		if err := checkEnvKey(key); err != nil {
			return nil, err
		}
		c := changes[key]
		ref := "secret:" + secretName(name, key)
		cur, has := pin.Env[key]
		isSecret := has && cur == ref
		switch {
		case c == nil:
			delete(pin.Env, key)
			if isSecret {
				drop = append(drop, secretName(name, key))
			}
			continue
		case c.Value == nil && c.Secret == nil:
			continue
		}
		secret := isSecret
		if c.Secret != nil {
			secret = *c.Secret
		}
		value := ""
		switch {
		case c.Value != nil:
			value = *c.Value
		case secret && has && !isSecret:
			value = cur // a plain value becomes a secret
		case isSecret && !secret:
			return nil, fmt.Errorf("%w: give a new value to make %s plain", ErrInvalid, key)
		case !has:
			return nil, fmt.Errorf("%w: %s has no value", ErrInvalid, key)
		default:
			continue
		}
		if pin.Env == nil {
			pin.Env = map[string]string{}
		}
		if secret {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%w: the secret %s has no value", ErrInvalid, key)
			}
			if err := secrets.Set(secretName(name, key), value); err != nil {
				return nil, err
			}
			pin.Env[key] = ref
			continue
		}
		pin.Env[key] = value
		if isSecret {
			drop = append(drop, secretName(name, key))
		}
	}
	if len(pin.Env) == 0 {
		pin.Env = nil
	}
	return drop, nil
}

// SetEnv changes the env of a harness and publishes harness.updated.
func (s *Service) SetEnv(ctx context.Context, name string, changes map[string]*EnvChange) error {
	if !Known(name) {
		return fmt.Errorf("unknown harness %q", name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !config.IsFirstClass(cfg.HarnessBase(name)) {
		return ErrNoConfigField
	}
	pin := cfg.Tools.Pin(name)
	if pin == nil {
		pin = &config.ToolPin{}
	}
	drop, err := applyEnv(name, pin, changes)
	if err != nil {
		return err
	}
	cfg.Tools.SetPin(name, pin)
	if err := cfg.Save(); err != nil {
		return err
	}
	s.dropSecrets(drop)
	return s.PublishUpdated(ctx, name, map[string]string{"env": strings.Join(slices.Sorted(maps.Keys(changes)), ",")})
}

func (s *Service) dropSecrets(names []string) {
	for _, n := range names {
		if err := secrets.Delete(n); err != nil {
			s.log.Warn("remove harness secret", "secret", n, "error", err)
		}
	}
}

// CreateVariant adds v to config.yaml, gives it to the visor, and publishes
// harness.created. The variant starts on.
func (s *Service) CreateVariant(ctx context.Context, v Variant) error {
	if err := config.ValidVariantName(v.Name); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !slices.Contains(config.VariantBaseNames(), v.Base) {
		return fmt.Errorf("%w: base must be one of %s", ErrInvalid, strings.Join(config.VariantBaseNames(), ", "))
	}
	if v.AuthMode != "" && v.AuthMode != AuthModeShared && v.AuthMode != AuthModeSeparate {
		return fmt.Errorf("%w: auth mode must be shared or separate", ErrInvalid)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if Known(v.Name) || cfg.Tools.Pin(v.Name) != nil {
		return ErrVariantExists
	}
	on := true
	pin := &config.ToolPin{Base: v.Base, Label: strings.TrimSpace(v.Label), Enabled: &on, Model: v.Model, Effort: v.Effort}
	if v.AuthMode == AuthModeSeparate {
		pin.Auth = AuthModeSeparate
	}
	if _, err := applyEnv(v.Name, pin, v.Env); err != nil {
		return err
	}
	cfg.Tools.SetPin(v.Name, pin)
	if err := cfg.Validate(); err != nil {
		cfg.Tools.SetPin(v.Name, nil)
		s.dropSecrets(ownedSecrets(v.Name))
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if err := s.persistMode(ctx, v.Name, cmp.Or(pin.Auth, AuthModeShared)); err != nil {
		s.log.Warn("give the new harness to the visor", "harness", v.Name, "error", err)
	}
	if _, err := s.Probe(ctx, v.Name, SourceProbe); err != nil {
		s.log.Warn("probe the new harness", "harness", v.Name, "error", err)
	}
	_, err = s.st.Append(ctx, store.BusEvent{
		Topic: "harness.created", Entity: "harness", EntityID: v.Name, Action: "created",
		Attrs: map[string]string{"harness": v.Name, "base": v.Base},
	})
	return err
}

// DeleteVariant removes a variant from config.yaml, with its secrets, its
// folder, and its login state. The caller moves its jobs first.
func (s *Service) DeleteVariant(ctx context.Context, name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !IsVariant(cfg, name) {
		return ErrNotVariant
	}
	cfg.Tools.SetPin(name, nil)
	if err := cfg.Save(); err != nil {
		return err
	}
	s.dropSecrets(ownedSecrets(name))
	if err := RemoveSecretEnv(name); err != nil {
		s.log.Warn("remove harness login key", "harness", name, "error", err)
	}
	if err := os.RemoveAll(config.HarnessDir(name)); err != nil {
		s.log.Warn("remove harness folder", "harness", name, "error", err)
	}
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM harness_state WHERE name=?`, name); err != nil {
		return err
	}
	_, err = s.st.Append(ctx, store.BusEvent{
		Topic: "harness.deleted", Entity: "harness", EntityID: name, Action: "deleted",
		Attrs: map[string]string{"harness": name},
	})
	return err
}

// ownedSecrets lists the secrets of the env of harness name.
func ownedSecrets(name string) []string {
	all, err := secrets.List()
	if err != nil {
		return nil
	}
	prefix := secretName(name, "")
	var out []string
	for _, n := range all {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}
