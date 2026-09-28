// Package license answers one question: is a feature allowed in this build.
// The verifier that checks a signed license file is Pro code behind
// ext.LicenseVerifier; this package owns the enum, the check, and the file.
package license

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/home"
)

// Feature is the closed enum of things a build can gate. ext carries
// features as strings on the wire; only this package knows the names.
type Feature string

const (
	FeatureEmail         Feature = "email"
	FeatureRemoteAccess  Feature = "remote_access"
	FeatureHostedIngress Feature = "hosted_ingress"
	FeatureBackup        Feature = "backup"
	FeatureRequests      Feature = "requests"
)

// All lists every feature in a fixed order.
func All() []Feature {
	return []Feature{FeatureEmail, FeatureRemoteAccess, FeatureHostedIngress, FeatureBackup, FeatureRequests}
}

// enforced is the release decision: a feature listed true needs a license
// that grants it. Nothing is enforced today; flipping a value is the
// release action, not a code change elsewhere. Requests are the first
// enforced feature.
var enforced = map[Feature]bool{ //nolint:gochecknoglobals // the release table; never mutated at runtime
	FeatureEmail:         false,
	FeatureRemoteAccess:  false,
	FeatureHostedIngress: false,
	FeatureBackup:        false,
	FeatureRequests:      true,
}

// Enforced reports whether this build gates f on a license.
func (f Feature) Enforced() bool { return enforced[f] }

var ErrNoVerifier = errors.New("this build has no license support")

// State is the license fact sheet for routes and the CLI.
type State struct {
	Supported bool       `json:"supported"` // this build can verify a license
	Source    string     `json:"source"`    // "file" or "none"
	Plan      string     `json:"plan,omitempty"`
	Features  []string   `json:"features,omitempty"`
	Subject   string     `json:"subject,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Allowed lists the features Has allows now, enforced or not. Clients
	// read it and never apply the rule themselves.
	Allowed []string `json:"allowed"`
}

// Service reads ~/.trebi/license.json and answers feature checks. With a
// nil verifier, which is every free build, every unenforced feature stays
// allowed and every enforced one is refused.
type Service struct {
	log      *slog.Logger
	mu       sync.Mutex
	verifier ext.LicenseVerifier
	loaded   bool
	ent      *ext.Entitlement
	source   string
}

func New(v ext.LicenseVerifier) *Service {
	return &Service{
		log:      slog.Default().With("source", "license"),
		verifier: v,
		source:   "none",
	}
}

// Has reports whether feature f is allowed here. The release table wins:
// an unenforced feature is free in every build, so a license that does not
// list it cannot revoke it. An enforced feature needs a valid grant.
func (s *Service) Has(f Feature) bool {
	if !f.Enforced() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		s.load()
	}
	if s.ent == nil {
		return false
	}
	for _, name := range s.ent.Features {
		if Feature(name) == f {
			return true
		}
	}
	return false
}

// State returns the loaded entitlement facts.
func (s *Service) State() State {
	allowed := []string{}
	for _, f := range All() {
		if s.Has(f) {
			allowed = append(allowed, string(f))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		s.load()
	}
	st := State{Supported: s.verifier != nil, Source: s.source, Allowed: allowed}
	if s.ent != nil {
		st.Plan = s.ent.Plan
		st.Features = s.ent.Features
		st.Subject = s.ent.Subject
		exp := s.ent.ExpiresAt
		st.ExpiresAt = &exp
	}
	return st
}

// Install verifies raw, writes it to the license path, and reloads.
func (s *Service) Install(raw []byte) error {
	if s.verifier == nil {
		return ErrNoVerifier
	}
	if _, err := s.verifier.Verify(raw, time.Now()); err != nil {
		return fmt.Errorf("license install: %w", err)
	}
	if err := os.WriteFile(home.LicensePath(), raw, 0o600); err != nil {
		return fmt.Errorf("license install: %w", err)
	}
	s.Reload()
	return nil
}

// Reload drops the cached entitlement so the next question re-reads the file.
func (s *Service) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	s.ent = nil
	s.source = "none"
}

func (s *Service) load() {
	s.loaded = true
	s.ent = nil
	s.source = "none"
	if s.verifier == nil {
		return
	}
	raw, err := os.ReadFile(home.LicensePath())
	if err != nil {
		return
	}
	ent, err := s.verifier.Verify(raw, time.Now())
	if err != nil {
		s.log.Info("license.rejected", "err", err.Error())
		return
	}
	s.ent = &ent
	s.source = "file"
}
