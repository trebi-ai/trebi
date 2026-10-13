package connectors

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

var holderRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// PackageSource is what a package install reads: a GitHub link (URL, and
// CheckID to reuse a check) or a catalog type (Catalog, and Version).
type PackageSource struct {
	URL     string `json:"url,omitempty"`
	CheckID string `json:"check_id,omitempty"`
	Catalog string `json:"catalog,omitempty"`
	Version string `json:"version,omitempty"`
}

// Package is one installed connector package. Version is the name of its
// folder: the catalog version, or <version>+g<commit> for a link.
type Package struct {
	Type        string   `json:"type"`
	Version     string   `json:"version"`
	Dir         string   `json:"dir"`
	Bin         string   `json:"bin"`
	Digest      string   `json:"digest"`
	InstalledAt string   `json:"installed_at,omitempty"`
	Connections []string `json:"connections"` // slugs of the connections that use it
	Holders     []string `json:"holders"`     // programs outside Trebi that keep it
}

// Ref is the type@version name of the package that GC and holds use.
func (p Package) Ref() string { return p.Type + "@" + p.Version }

func (p PackageSource) validate() error {
	var bad []FieldError
	switch {
	case (p.URL == "") == (p.Catalog == ""):
		bad = append(bad, FieldError{Field: "source", Message: "give url or catalog: one of the two"})
	case p.URL != "" && p.Version != "":
		bad = append(bad, FieldError{Field: "source.version", Message: "a link installs the version of its commit"})
	case p.Catalog != "" && p.CheckID != "":
		bad = append(bad, FieldError{Field: "source.check_id", Message: "only for a link"})
	}
	if len(bad) > 0 {
		return &ValidationError{Name: "package", Fields: bad}
	}
	return nil
}

// InstallPackage installs one connector package from the catalog or from a
// link. It creates no connection and starts no connection process. A link
// gets the same check and conformance suites as a link install.
func (s *Service) InstallPackage(ctx context.Context, src PackageSource) (Package, error) {
	if err := src.validate(); err != nil {
		return Package{}, err
	}
	var typ, version, digest string
	if src.URL != "" {
		st, err := s.stageInstall(ctx, SourceInstall{URL: src.URL, CheckID: src.CheckID})
		if err != nil {
			return Package{}, err
		}
		defer s.dropCheck(st.check.ID)
		if err := s.GuardReserved(st.manifest.Name); err != nil {
			return Package{}, err
		}
		if _, err := s.buildLink(ctx, st, false, true); err != nil {
			return Package{}, err
		}
		typ, version, digest = st.manifest.Name, linkPackage(st.manifest.Version, st.check.Source.Commit), st.digest
	} else {
		if s.catalog == nil {
			return Package{}, ErrNoCatalog
		}
		if err := s.GuardReserved(src.Catalog); err != nil {
			return Package{}, err
		}
		v, err := s.pickVersion(ctx, src.Catalog, src.Version, "", "")
		if err != nil {
			return Package{}, err
		}
		if _, digest, err = s.installVersion(ctx, src.Catalog, v); err != nil {
			return Package{}, err
		}
		typ, version = src.Catalog, v
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.package.installed", Entity: "connector_package", EntityID: typ + "@" + version, Action: "installed",
		Attrs: map[string]string{"type": typ, "version": version, "digest": digest},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.package.installed", "error", err)
	}
	pkgs, err := s.Packages(ctx)
	if err != nil {
		return Package{}, err
	}
	if i := slices.IndexFunc(pkgs, func(p Package) bool { return p.Type == typ && p.Version == version }); i >= 0 {
		return pkgs[i], nil
	}
	return Package{Type: typ, Version: version, Dir: home.PackageDir(typ, version), Digest: digest, Connections: []string{}, Holders: []string{}}, nil
}

// Packages lists the installed packages with the connections and the
// holders that keep each one.
func (s *Service) Packages(ctx context.Context) ([]Package, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT type, version, digest, bin_path, installed_at FROM connector_installs ORDER BY type, version`)
	if err != nil {
		return nil, fmt.Errorf("list packages: %w", err)
	}
	defer rows.Close()
	out := []Package{}
	for rows.Next() {
		p := Package{Connections: []string{}, Holders: []string{}}
		if err := rows.Scan(&p.Type, &p.Version, &p.Digest, &p.Bin, &p.InstalledAt); err != nil {
			return nil, err
		}
		p.Dir = home.PackageDir(p.Type, p.Version)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	holds, err := s.holds(ctx)
	if err != nil {
		return nil, err
	}
	conns := map[string][]string{}
	for _, c := range s.snapshot() {
		if c.From == FromCatalog || c.From == FromGitHub {
			ref := c.Type + "@" + c.packageVersion()
			conns[ref] = append(conns[ref], c.Slug)
		}
	}
	for i := range out {
		ref := out[i].Ref()
		if cs := conns[ref]; cs != nil {
			slices.Sort(cs)
			out[i].Connections = cs
		}
		if hs := holds[ref]; hs != nil {
			out[i].Holders = hs
		}
	}
	return out, nil
}

// KeepPackages replaces the packages that one holder keeps. GC does not
// remove a package that a holder keeps. Each package is type@version; it
// need not be installed yet.
func (s *Service) KeepPackages(ctx context.Context, holder string, pkgs []string) ([]string, error) {
	var bad []FieldError
	if !holderRE.MatchString(holder) {
		bad = append(bad, FieldError{Field: "holder", Message: "must match " + holderRE.String()})
	}
	keep := []string{}
	for _, p := range pkgs {
		typ, version, ok := strings.Cut(p, "@")
		if !ok || typ == "" || version == "" || strings.ContainsAny(p, `/\`) {
			bad = append(bad, FieldError{Field: "packages", Message: fmt.Sprintf("%q is not type@version", p)})
			continue
		}
		if !slices.Contains(keep, p) {
			keep = append(keep, p)
		}
	}
	if len(bad) > 0 {
		return nil, &ValidationError{Name: "keep", Fields: bad}
	}
	slices.Sort(keep)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM connector_package_holds WHERE holder = ?`, holder); err != nil {
			return err
		}
		for _, p := range keep {
			typ, version, _ := strings.Cut(p, "@")
			if _, err := tx.ExecContext(ctx, `INSERT INTO connector_package_holds (holder, type, version) VALUES (?, ?, ?)`, holder, typ, version); err != nil {
				return err
			}
		}
		return nil
	}, store.BusEvent{
		Topic: "connector.package.kept", Entity: "connector_package", EntityID: holder, Action: "kept",
		Attrs: map[string]string{"holder": holder, "packages": fmt.Sprint(len(keep))},
	})
	if err != nil {
		return nil, fmt.Errorf("keep packages of %s: %w", holder, err)
	}
	return keep, nil
}

// holds maps type@version to the sorted holders that keep it.
func (s *Service) holds(ctx context.Context) (map[string][]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT holder, type, version FROM connector_package_holds ORDER BY holder`)
	if err != nil {
		return nil, fmt.Errorf("package holds: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var holder, typ, version string
		if err := rows.Scan(&holder, &typ, &version); err != nil {
			return nil, err
		}
		out[typ+"@"+version] = append(out[typ+"@"+version], holder)
	}
	return out, rows.Err()
}
