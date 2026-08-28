package config

import "testing"

// The seeded file must load under the strict decoder and validate, and must
// produce the same effective config as no file at all.
func TestSeedTemplateLoads(t *testing.T) {
	cfg := defaultConfig()
	if err := decodeConfig([]byte(seedTemplate), cfg); err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	applyLoadDefaults(cfg)
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate seed: %v", err)
	}

	bare := defaultConfig()
	applyLoadDefaults(bare)
	if cfg.Defaults.Command != bare.Defaults.Command {
		t.Errorf("command: seed %q != default %q", cfg.Defaults.Command, bare.Defaults.Command)
	}
	if cfg.Terminal != bare.Terminal {
		t.Errorf("terminal: seed %q != default %q", cfg.Terminal, bare.Terminal)
	}
	if cfg.Tools.Fallback != bare.Tools.Fallback {
		t.Errorf("tools.fallback: seed %q != default %q", cfg.Tools.Fallback, bare.Tools.Fallback)
	}
}
