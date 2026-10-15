package connectors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors/conformance"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"gopkg.in/yaml.v3"
)

// Groups of a SourceResult.
const (
	GroupManifest = "manifest"
	GroupValid    = "valid"
	GroupSkill    = "skill"
	GroupEvents   = "events"
	GroupProgram  = "program"
	GroupInputs   = "inputs"
)

// checkTTL is how long a check and its staging folder stay valid.
const checkTTL = time.Hour

// maxCheckFiles caps SourceCheck.Files.
const maxCheckFiles = 50

// Messages of the check that the UI shows as they are.
const (
	msgNoManifest = "No connector settings found in this folder."
	msgNoCatalog  = "A link has no catalog build. Add install.build or install.github_release."
)

var unknownFieldRE = regexp.MustCompile(`field (\S+) not found`)

// SourceCheck is the report of one link before an install.
type SourceCheck struct {
	ID         string         `json:"id"` // chk_<ulid>
	Source     GitHubSource   `json:"source"`
	URL        string         `json:"url"`
	Entry      *SourceEntry   `json:"entry"` // nil when no manifest loads
	Files      []string       `json:"files"` // top-level names of the folder; a folder ends with /
	Found      []string       `json:"found"` // sub-folders with a manifest, when the folder has none
	Results    []SourceResult `json:"results"`
	Runs       SourceRuns     `json:"runs"`
	CanInstall bool           `json:"can_install"`
	ExpiresAt  time.Time      `json:"expires_at"`
}

// SourceEntry is what the manifest of a link says about the connector.
type SourceEntry struct {
	Name         string           `json:"name"`
	Title        string           `json:"title"`
	Description  string           `json:"description"`
	Version      string           `json:"version"`
	Capabilities []string         `json:"capabilities"`
	Inputs       []manifest.Input `json:"inputs"`
}

// SourceResult is one row of a check. Status is a conformance status.
type SourceResult struct {
	Group   string `json:"group"`
	Check   string `json:"check"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// SourceRuns lists what the connector runs on this computer.
type SourceRuns struct {
	Program    string   `json:"program"`    // install.bin
	Commands   []string `json:"commands"`   // actions.cli commands and events.command
	Operations []string `json:"operations"` // "name (state)"
	Method     string   `json:"method"`     // the install method that applies
}

// staged is one check with its folder, kept until it expires.
type staged struct {
	check    SourceCheck
	dir      string
	digest   string
	manifest manifest.Manifest
}

// CheckSource fetches the folder of a GitHub link into a staging folder and
// checks it. It installs nothing and starts no program.
func (s *Service) CheckSource(ctx context.Context, link string) (SourceCheck, error) {
	st, err := s.stageLink(ctx, link)
	if err != nil {
		return SourceCheck{}, err
	}
	return st.check, nil
}

// SweepStaging removes the staging folders and the checks that expired.
func (s *Service) SweepStaging() {
	now := s.now()
	s.mu.Lock()
	for id, st := range s.checks {
		if now.After(st.check.ExpiresAt) {
			delete(s.checks, id)
		}
	}
	live := map[string]bool{}
	for id := range s.checks {
		live[id] = true
	}
	s.mu.Unlock()
	ents, err := os.ReadDir(home.ConnectorStagingRoot())
	if err != nil {
		return
	}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || live[e.Name()] || now.Sub(fi.ModTime()) < checkTTL {
			continue
		}
		if err := os.RemoveAll(filepath.Join(home.ConnectorStagingRoot(), e.Name())); err != nil {
			s.log.Warn("connector staging", "check", e.Name(), "error", err)
		}
	}
}

// stageLink parses and resolves a link, then checks its commit.
func (s *Service) stageLink(ctx context.Context, link string) (staged, error) {
	if s.gh == nil {
		return staged{}, &SourceError{Kind: ErrSourceUnavailable, Message: "Trebi cannot read links: the link client is not set up."}
	}
	src, refPath, err := ParseGitHubLink(link)
	if err != nil {
		return staged{}, err
	}
	if src, err = s.gh.Resolve(ctx, src, refPath); err != nil {
		return staged{}, err
	}
	return s.stageCommit(ctx, src)
}

// stageCommit fetches src at its commit into a new staging folder and checks
// it. The check stays for checkTTL.
func (s *Service) stageCommit(ctx context.Context, src GitHubSource) (staged, error) {
	s.SweepStaging()
	id := "chk_" + strings.ToLower(ulid.Make().String())
	dir := home.ConnectorStagingDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return staged{}, err
	}
	digest, err := s.gh.Fetch(ctx, src, dir)
	if err != nil {
		_ = os.RemoveAll(dir) // best effort; the sweep removes a leftover
		return staged{}, err
	}
	chk := SourceCheck{
		ID: id, Source: src, URL: src.URL(), Files: topFiles(dir), Found: []string{},
		Results: []SourceResult{}, ExpiresAt: s.now().Add(checkTTL).UTC(),
		Runs: SourceRuns{Commands: []string{}, Operations: []string{}},
	}
	m := chk.inspect(ctx, dir)
	st := staged{check: chk, dir: dir, digest: digest, manifest: m}
	s.mu.Lock()
	s.checks[id] = st
	s.mu.Unlock()
	return st, nil
}

// stagedCheck returns a check that has not expired and that holds commit.
func (s *Service) stagedCheck(id, commit string) (staged, bool) {
	s.mu.Lock()
	st, ok := s.checks[id]
	s.mu.Unlock()
	if !ok || s.now().After(st.check.ExpiresAt) || st.check.Source.Commit != commit {
		return staged{}, false
	}
	if _, err := os.Stat(st.dir); err != nil {
		return staged{}, false
	}
	return st, true
}

// dropCheck forgets a check and removes its folder.
func (s *Service) dropCheck(id string) {
	s.mu.Lock()
	delete(s.checks, id)
	s.mu.Unlock()
	_ = os.RemoveAll(home.ConnectorStagingDir(id)) // best effort; the sweep removes a leftover
}

// inspect fills the rows, the entry, and the runs from the folder dir. It
// returns the manifest, or the zero value when none loads.
func (c *SourceCheck) inspect(ctx context.Context, dir string) manifest.Manifest {
	defer func() {
		c.CanInstall = !slices.ContainsFunc(c.Results, func(r SourceResult) bool { return r.Status == conformance.StatusFail })
	}()
	data, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		c.Found = findManifests(dir)
		c.add(GroupManifest, "file", conformance.StatusFail, msgNoManifest, "")
		return manifest.Manifest{}
	}
	c.add(GroupManifest, "file", conformance.StatusPass, "", "")
	m, ok := c.decode(data)
	if !ok {
		return manifest.Manifest{}
	}
	c.Entry = &SourceEntry{
		Name: m.Name, Title: m.Title, Description: m.Description, Version: m.Version,
		Capabilities: m.Capabilities(), Inputs: []manifest.Input{},
	}
	if m.Setup != nil {
		c.Entry.Inputs = m.Setup.Inputs
	}
	c.suiteRows(ctx, m, dir)
	c.program(m)
	c.inputs(m)
	return m
}

// decode reads the manifest strictly: one row for each unknown key and one
// for each rule that fails. It reports whether the manifest is usable.
func (c *SourceCheck) decode(data []byte) (manifest.Manifest, bool) {
	m, err := manifest.Parse(data)
	if err != nil {
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			c.add(GroupValid, "decode", conformance.StatusFail, err.Error(), "")
			return manifest.Manifest{}, false
		}
		for _, msg := range te.Errors {
			field := ""
			if sm := unknownFieldRE.FindStringSubmatch(msg); sm != nil {
				field = sm[1]
			}
			c.add(GroupValid, "key", conformance.StatusFail, msg, field)
		}
		if err := yaml.Unmarshal(data, &m); err != nil {
			return manifest.Manifest{}, false
		}
	}
	verr := m.Validate()
	var ve *ValidationError
	switch {
	case errors.As(verr, &ve):
		for _, fe := range ve.Fields {
			c.add(GroupValid, "rule", conformance.StatusFail, fe.Message, fe.Field)
		}
	case verr != nil:
		c.add(GroupValid, "rule", conformance.StatusFail, verr.Error(), "")
	case err == nil:
		c.add(GroupValid, "rules", conformance.StatusPass, "", "")
	}
	return m, verr == nil && err == nil
}

// suiteRows runs the manifest suite on the folder and files its rows under
// the skill, events, and program groups. A skill compat miss is a warn.
func (c *SourceCheck) suiteRows(ctx context.Context, m manifest.Manifest, dir string) {
	rows := conformance.NewRunner(nil).Run(ctx, conformance.Target{Manifest: m, Dir: dir}, []string{conformance.SuiteManifest})
	for _, r := range rows {
		switch r.Check {
		case "event schemas":
			c.add(GroupEvents, r.Check, r.Status, r.Message, "")
		case "skill":
			c.add(GroupSkill, r.Check, r.Status, r.Message, "")
		case "skill compat":
			st := r.Status
			if st == conformance.StatusFail {
				st = conformance.StatusWarn
			}
			c.add(GroupSkill, r.Check, st, r.Message, "")
		case "build module":
			c.add(GroupProgram, r.Check, r.Status, r.Message, "")
		}
	}
}

// program checks that an install method works for a link, and fills Runs.
// A link has no catalog build.
func (c *SourceCheck) program(m manifest.Manifest) {
	if m.Actions != nil && m.Actions.CLI != nil {
		c.Runs.Commands = append(c.Runs.Commands, m.Actions.CLI.Commands...)
	}
	if m.Events != nil && m.Events.Command != "" {
		c.Runs.Commands = append(c.Runs.Commands, m.Events.Command)
	}
	for _, op := range m.Operations() {
		c.Runs.Operations = append(c.Runs.Operations, fmt.Sprintf("%s (%s)", op.Name, op.DefaultState()))
	}
	in := m.Install
	if in == nil || in.Bin == "" {
		c.add(GroupProgram, "method", conformance.StatusSkip, "The connector installs no program.", "")
		return
	}
	c.Runs.Program = in.Bin
	link := *in
	link.Catalog = false
	if in.GitHubRelease == nil && in.Build == nil && in.Brew == "" && in.Go == "" {
		c.add(GroupProgram, "method", conformance.StatusFail, msgNoCatalog, "install")
		return
	}
	lm := m
	lm.Install = &link
	r := conformance.InstallMethod(lm)
	c.add(GroupProgram, r.Check, r.Status, r.Message, "")
	switch {
	case link.GitHubRelease != nil:
		c.Runs.Method = "github_release"
	case link.Build != nil:
		c.Runs.Method = "build"
	case link.Brew != "":
		c.Runs.Method = "brew"
	default:
		c.Runs.Method = "go"
	}
}

// inputs checks the labels of the required inputs and counts them.
func (c *SourceCheck) inputs(m manifest.Manifest) {
	if m.Setup == nil || len(m.Setup.Inputs) == 0 {
		c.add(GroupInputs, "required", conformance.StatusPass, "The connector needs no settings.", "")
		return
	}
	var required, secret, unlabeled []string
	for _, in := range m.Setup.Inputs {
		if in.Secret {
			secret = append(secret, in.Name)
		}
		if !in.Required {
			continue
		}
		required = append(required, in.Name)
		if strings.TrimSpace(in.Label) == "" {
			unlabeled = append(unlabeled, in.Name)
		}
	}
	if len(unlabeled) > 0 {
		c.add(GroupInputs, "labels", conformance.StatusFail, "Required settings with no label: "+strings.Join(unlabeled, ", "), "setup.inputs")
	} else {
		c.add(GroupInputs, "labels", conformance.StatusPass, "", "")
	}
	msg := fmt.Sprintf("%d required settings.", len(required))
	if len(secret) > 0 {
		msg += " Secrets: " + strings.Join(secret, ", ") + "."
	}
	c.add(GroupInputs, "required", conformance.StatusPass, msg, "")
}

func (c *SourceCheck) add(group, check, status, msg, field string) {
	c.Results = append(c.Results, SourceResult{Group: group, Check: check, Status: status, Message: msg, Field: field})
}

// topFiles lists the top-level names of dir, sorted, at most maxCheckFiles.
func topFiles(dir string) []string {
	out := []string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if len(out) == maxCheckFiles {
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	return out
}

// findManifests lists the sub-folders of dir, to depth 2, that hold a
// manifest, as slash paths.
func findManifests(dir string) []string {
	out := []string{}
	for _, pattern := range []string{"*", "*/*"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern, manifest.FileName))
		if err != nil {
			continue
		}
		for _, m := range matches {
			rel, err := filepath.Rel(dir, filepath.Dir(m))
			if err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
	}
	slices.Sort(out)
	return out
}

// Reasons of an InstallCheckError.
const (
	ReasonCheckFailed       = "check_failed"
	ReasonConformanceFailed = "conformance_failed"
)

// InstallCheckError blocks a link install. Results holds the check rows
// ([]SourceResult) or the conformance rows ([]conformance.Result).
type InstallCheckError struct {
	Reason  string
	Results any
}

func (e *InstallCheckError) Error() string {
	if e.Reason == ReasonConformanceFailed {
		return "the connector failed its tests; see the results"
	}
	return "the link check failed; see the results"
}

// installLink installs a connection from a GitHub link: the check, the
// program, the conformance suites in the sandbox, then the connection.
func (s *Service) installLink(ctx context.Context, req InstallRequest) (Connection, error) {
	var bad []FieldError
	switch {
	case req.Source == nil:
		bad = append(bad, FieldError{Field: "force", Message: "only for an install from a link"})
	case req.Name != "":
		bad = append(bad, FieldError{Field: "name", Message: "use name or source, not both"})
	}
	if req.Track != "" {
		bad = append(bad, FieldError{Field: "track", Message: "a link stays on its commit"})
	}
	if req.Version != "" {
		bad = append(bad, FieldError{Field: "version", Message: "a link installs the version of its commit"})
	}
	if len(bad) > 0 {
		return Connection{}, &ValidationError{Name: "install", Fields: bad}
	}
	st, err := s.stageInstall(ctx, *req.Source)
	if err != nil {
		return Connection{}, err
	}
	defer s.dropCheck(st.check.ID)
	m := st.manifest
	if err := s.GuardReserved(m.Name); err != nil {
		return Connection{}, err
	}
	if req.Slug != "" {
		if err := s.GuardReserved(req.Slug); err != nil {
			return Connection{}, err
		}
		if !s.slugFree(req.Slug, "") {
			return Connection{}, fmt.Errorf("%w: slug %q", ErrExists, req.Slug)
		}
	}
	created, err := s.buildLink(ctx, st, req.Force, true)
	if err != nil {
		return Connection{}, err
	}
	src := st.check.Source
	c, err := s.Create(ctx, Connection{
		Type: m.Name, Slug: req.Slug, Title: req.Title, From: FromGitHub, Source: &src,
		Version: m.Version, Digest: st.digest, Inputs: req.Inputs, Scope: req.Scope,
	}.WithDefaultScope(req.Workspace))
	if err != nil {
		if created {
			s.removeVersion(ctx, m.Name, linkPackage(m.Version, src.Commit))
		}
		s.step(ctx, st.check.ID, "start", conformance.StatusFail)
		return Connection{}, err
	}
	s.step(ctx, st.check.ID, "start", conformance.StatusPass)
	s.appendBus(ctx, "connector.installed", c.ID, map[string]string{
		"version": c.Version, "type": c.Type, "slug": c.Slug, "source": FromGitHub, "repo": src.Repo, "commit": src.Commit,
	})
	return c, nil
}

// stageInstall returns the check of an install: the check of CheckID when
// it holds the commit that the link has now, else a new check. A check that
// fails is an InstallCheckError.
func (s *Service) stageInstall(ctx context.Context, in SourceInstall) (staged, error) {
	if s.gh == nil {
		return staged{}, &SourceError{Kind: ErrSourceUnavailable, Message: "Trebi cannot read links: the link client is not set up."}
	}
	src, refPath, err := ParseGitHubLink(in.URL)
	if err != nil {
		return staged{}, err
	}
	if src, err = s.gh.Resolve(ctx, src, refPath); err != nil {
		return staged{}, err
	}
	st, ok := s.stagedCheck(in.CheckID, src.Commit)
	if !ok || st.check.Source.Path != src.Path || st.check.Source.Repo != src.Repo {
		if st, err = s.stageCommit(ctx, src); err != nil {
			return staged{}, err
		}
	}
	s.step(ctx, st.check.ID, "download", conformance.StatusPass)
	if !st.check.CanInstall {
		s.dropCheck(st.check.ID)
		return staged{}, &InstallCheckError{Reason: ReasonCheckFailed, Results: st.check.Results}
	}
	return st, nil
}

// buildLink moves the staging folder of st into a temp folder, installs the
// program, runs the conformance suites in the sandbox, and puts the folder
// in place. full runs every suite and emits the steps; else only the install
// suite runs. A package with the same digest stays. It reports whether it
// made the package folder.
func (s *Service) buildLink(ctx context.Context, st staged, force, full bool) (bool, error) {
	m, id := st.manifest, st.check.ID
	pkg := linkPackage(m.Version, st.check.Source.Commit)
	if packageDigest(m.Name, pkg) == st.digest {
		return false, nil
	}
	parent := filepath.Dir(home.PackageDir(m.Name, pkg))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, err
	}
	tmp := filepath.Join(parent, ".install-"+id)
	if err := os.Rename(st.dir, tmp); err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best effort; empty after the rename
	s.step(ctx, id, "program", "running")
	binPath, err := s.installBinary(ctx, m, tmp, true)
	if err != nil {
		s.step(ctx, id, "program", conformance.StatusFail)
		return false, fmt.Errorf("install %s from %s: %w", m.Name, st.check.Source.Repo, err)
	}
	s.step(ctx, id, "program", conformance.StatusPass)
	rows := s.testLink(ctx, m, tmp, id, full)
	if worst(rows) == conformance.StatusFail && !force {
		return false, &InstallCheckError{Reason: ReasonConformanceFailed, Results: rows}
	}
	if err := s.placePackage(ctx, m.Name, pkg, tmp, st.digest, binPath); err != nil {
		return false, err
	}
	return true, nil
}

// testLink runs the conformance suites on the package folder dir, one bus
// step for each group when full.
func (s *Service) testLink(ctx context.Context, m manifest.Manifest, dir, id string, full bool) []conformance.Result {
	groups := []struct {
		step   string
		suites []string
	}{
		{"test.protocol", []string{conformance.SuiteManifest, conformance.SuiteInstall, conformance.SuiteInputs, conformance.SuiteProtocol}},
		{"test.actions", []string{"actions"}},
		{"test.events", []string{conformance.SuiteEventsLines}},
		{"test.folder", []string{conformance.SuiteFolder}},
	}
	if !full {
		groups = groups[:1]
		groups[0].suites = []string{conformance.SuiteInstall}
	}
	runner := conformance.NewRunner(s.log)
	out := []conformance.Result{}
	for _, g := range groups {
		if full {
			s.step(ctx, id, g.step, "running")
		}
		rows := runner.Run(ctx, conformance.Target{Manifest: m, Dir: dir}, g.suites)
		if full {
			s.step(ctx, id, g.step, worst(rows))
		}
		out = append(out, rows...)
	}
	return out
}

// step emits one connector.install.step event of a check.
func (s *Service) step(ctx context.Context, checkID, step, status string) {
	s.appendBus(ctx, "connector.install.step", checkID, map[string]string{"check_id": checkID, "step": step, "status": status})
}

// ensureLinkPackage restores the missing package of a link connection from
// its pinned commit. Another tree at that commit is ErrDigestMismatch.
func (s *Service) ensureLinkPackage(ctx context.Context, c Connection) error {
	pkg := c.packageVersion()
	if have := packageDigest(c.Type, pkg); have != "" {
		if have != c.Digest {
			return fmt.Errorf("%w: connector %q: the package %s %s has digest %s, not %s", ErrDigestMismatch, c.Slug, c.Type, pkg, have, c.Digest)
		}
		return nil
	}
	if s.gh == nil || c.Source == nil {
		return fmt.Errorf("%w: %s %s", ErrNotInstalled, c.Type, pkg)
	}
	st, err := s.stageCommit(ctx, *c.Source)
	if err != nil {
		return err
	}
	defer s.dropCheck(st.check.ID)
	if st.digest != c.Digest {
		return fmt.Errorf("%w: connector %q: %s has digest %s, not %s", ErrDigestMismatch, c.Slug, c.Source.URL(), st.digest, c.Digest)
	}
	if st.manifest.Name != c.Type || st.manifest.Version != c.Version {
		return fmt.Errorf("connector %q: %s holds %s@%s, not %s@%s", c.Slug, c.Source.URL(), st.manifest.Name, st.manifest.Version, c.Type, c.Version)
	}
	_, err = s.buildLink(ctx, st, false, false)
	return err
}

// checkLink compares the branch head of a link connection with its commit.
// A new commit is recorded with the version of its manifest; it never
// applies on its own. A link to a commit is skipped.
func (s *Service) checkLink(ctx context.Context, c Connection) (*UpdateResult, error) {
	if c.Source == nil || c.Source.pinned() || s.gh == nil {
		return nil, nil //nolint:nilnil // nothing to check
	}
	head, err := s.gh.Head(ctx, *c.Source)
	s.mu.Lock()
	if errors.Is(err, ErrSourceNotFound) {
		s.srcErr[c.ID] = err
	} else if err == nil {
		delete(s.srcErr, c.ID)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if head == c.Source.Commit {
		s.setUpdate(c.ID, Pending{})
		return nil, nil //nolint:nilnil // no update is not an error
	}
	version := c.Version
	if data, err := s.gh.Manifest(ctx, *c.Source, head); err == nil {
		if m, err := manifest.Parse(data); err == nil && m.Version != "" {
			version = m.Version
		}
	}
	if s.setUpdate(c.ID, Pending{Version: version, Commit: head}) {
		s.appendBus(ctx, "connector.update.available", c.ID, map[string]string{"version": version, "commit": head, "current": c.Version, "slug": c.Slug})
	}
	return &UpdateResult{ID: c.ID, Slug: c.Slug, From: c.Version, To: version, Commit: head}, nil
}

// updateLink moves a link connection to the head of its branch: a new
// check, the install next to the current package, the move, and GC. A link
// takes the version of its commit, so a want other than "" must name the
// head: its manifest version or its package name.
func (s *Service) updateLink(ctx context.Context, c Connection, want string) error {
	if s.gh == nil || c.Source == nil {
		return &SourceError{Kind: ErrSourceUnavailable, Message: "Trebi cannot read links: the link client is not set up."}
	}
	head, err := s.gh.Head(ctx, *c.Source)
	if err != nil {
		return err
	}
	if head == c.Source.Commit {
		if err := checkLinkWant(c.Slug, want, c.Version, head); err != nil {
			return err
		}
		s.setUpdate(c.ID, Pending{})
		return nil
	}
	src := *c.Source
	src.Commit = head
	st, err := s.stageCommit(ctx, src)
	if err != nil {
		return err
	}
	defer s.dropCheck(st.check.ID)
	if !st.check.CanInstall {
		return &InstallCheckError{Reason: ReasonCheckFailed, Results: st.check.Results}
	}
	if err := checkLinkWant(c.Slug, want, st.manifest.Version, head); err != nil {
		return err
	}
	if st.manifest.Name != c.Type {
		return &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "source", Message: fmt.Sprintf("the link now holds connector %q, not %q", st.manifest.Name, c.Type)}}}
	}
	created, err := s.buildLink(ctx, st, false, true)
	if err != nil {
		return err
	}
	from := c.Version
	if err := s.setVersion(ctx, c.ID, st.manifest.Version, st.digest, &src); err != nil {
		if created {
			s.removeVersion(ctx, c.Type, linkPackage(st.manifest.Version, head))
		}
		return err
	}
	s.setUpdate(c.ID, Pending{})
	s.appendBus(ctx, "connector.updated.version", c.ID, map[string]string{"from": from, "to": st.manifest.Version, "commit": head, "slug": c.Slug})
	if _, err := s.GC(ctx, false); err != nil {
		s.log.Warn("connector gc", "error", err)
	}
	return nil
}

// linkPackage is the package folder name of a link: the version and the
// first 12 hex of the commit.
func linkPackage(version, commit string) string {
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return version + "+g" + commit
}

// checkLinkWant accepts a want that is "", the version of the head, or its
// package name. Anything else is a validation error.
func checkLinkWant(slug, want, version, head string) error {
	pkg := linkPackage(version, head)
	if want == "" || want == version || want == pkg {
		return nil
	}
	return &ValidationError{Name: slug, Fields: []FieldError{{Field: "version", Message: fmt.Sprintf("the link takes the version of its branch head, %s, not %s", pkg, want)}}}
}

// worst is the worst status of rows: fail, warn, else pass.
func worst(rows []conformance.Result) string {
	st := conformance.StatusPass
	for _, r := range rows {
		switch r.Status {
		case conformance.StatusFail:
			return conformance.StatusFail
		case conformance.StatusWarn:
			st = conformance.StatusWarn
		}
	}
	return st
}
