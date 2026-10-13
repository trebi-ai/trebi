package connectors

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
)

// commandLinks is the wanted state of home.ConnectorCommandsDir: one link for
// each command that an installed connector declares. The link points into
// the bin folder of the version in use, so it follows each update.
type commandLinks struct {
	want    map[string]linkTarget // command name -> target
	clashes []linkClash
}

type linkTarget struct {
	path    string
	typ     string
	version manifest.Version
}

// linkClash is a command that two connector types declare. The type that
// sorts first gets the link.
type linkClash struct {
	Command string
	Linked  string // the type that has the link
	Skipped string
	ID      string // the connection of the skipped type
}

// newCommandLinks plans the links of the installed connections. Only a
// catalog or link connection has a package bin folder. For one type on two
// versions, the newest version gets the link.
func newCommandLinks(rs []Resolved) commandLinks {
	l := commandLinks{want: map[string]linkTarget{}}
	slices.SortFunc(rs, func(a, b Resolved) int { return cmp.Compare(a.Type, b.Type) })
	for _, r := range rs {
		bin := r.BinDir()
		if bin == "" || (r.From != FromCatalog && r.From != FromGitHub) {
			continue
		}
		ver, _ := manifest.ParseVersion(r.Version) // a bad version sorts as zero
		for _, cmd := range declaredCommands(r.Manifest) {
			path := filepath.Join(bin, cmd)
			if st, err := os.Stat(path); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
				continue
			}
			t := linkTarget{path: path, typ: r.Type, version: ver}
			cur, ok := l.want[cmd]
			switch {
			case !ok:
				l.want[cmd] = t
			case cur.typ != r.Type:
				l.clashes = append(l.clashes, linkClash{Command: cmd, Linked: cur.typ, Skipped: r.Type, ID: r.ID})
			case ver.CompareFull(cur.version) > 0:
				l.want[cmd] = t
			}
		}
	}
	return l
}

// declaredCommands lists the bare command names of a manifest: the program,
// the CLI actions, the MCP server, the events command, and the operations.
func declaredCommands(m manifest.Manifest) []string {
	var out []string
	if m.Install != nil {
		out = append(out, m.Install.Bin)
	}
	if m.Actions != nil {
		if m.Actions.CLI != nil {
			out = append(out, m.Actions.CLI.Commands...)
		}
		if m.Actions.MCP != nil {
			out = append(out, m.Actions.MCP.Command)
		}
	}
	if m.Events != nil {
		out = append(out, m.Events.Command)
	}
	out = append(out, m.OperationBinaries()...)
	return slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(out))), func(c string) bool {
		return c == "" || c == "trebi" || c != filepath.Base(c) || c[0] == '.'
	})
}

// apply makes dir match the plan. It removes each link that is not wanted
// and never touches a file that is not a link.
func (l commandLinks) apply(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		if _, ok := l.want[e.Name()]; !ok {
			errs = append(errs, os.Remove(filepath.Join(dir, e.Name())))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(l.want)) {
		link, target := filepath.Join(dir, name), l.want[name].path
		if st, err := os.Lstat(link); err == nil && st.Mode()&fs.ModeSymlink == 0 {
			errs = append(errs, fmt.Errorf("%s is not a link; it stays", link))
			continue
		}
		if got, err := os.Readlink(link); err == nil && got == target {
			continue
		}
		tmp := filepath.Join(dir, "."+name+".tmp")
		_ = os.Remove(tmp) // a leftover of a crash
		if err := os.Symlink(target, tmp); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, os.Rename(tmp, link))
	}
	return errors.Join(errs...)
}

// linkFindings reports each command that has no link because of a clash.
func (s *Service) linkFindings() []Finding {
	var out []Finding
	for _, c := range newCommandLinks(s.installedResolved()).clashes {
		out = append(out, Finding{c.ID, "warn", "connector_command_clash",
			fmt.Sprintf("connectors %s and %s both have the command %q; only %s is in %s", c.Linked, c.Skipped, c.Command, c.Linked, home.ConnectorCommandsDir())})
	}
	return out
}

// installedResolved resolves every connection that loads.
func (s *Service) installedResolved() []Resolved {
	var out []Resolved
	for _, c := range s.snapshot() {
		if r, err := resolveConnection(c, "", nil); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// syncCommandLinks makes the command links match the connections.
func (s *Service) syncCommandLinks(ctx context.Context) {
	if err := newCommandLinks(s.installedResolved()).apply(home.ConnectorCommandsDir()); err != nil {
		s.log.WarnContext(ctx, "connector command links", "error", err)
	}
}
