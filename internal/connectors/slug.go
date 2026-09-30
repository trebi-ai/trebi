package connectors

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/home"
	"gopkg.in/yaml.v3"
)

// ErrSlugRewrite is a slug change that could not rewrite a reference file.
// The files that changed are put back, and the slug stays.
var ErrSlugRewrite = errors.New("slug references could not be rewritten")

// slugRewrite changes the slug references in the config files: job and
// agent connectors: lists and trigger event: values, workspace layer keys in
// trebi.yaml, and notify via: steps. It edits the scalar in place, so the
// rest of each file keeps its form.
type slugRewrite struct {
	old, new string
	done     []fileBackup
}

type fileBackup struct {
	path string
	data []byte
	mode os.FileMode
}

// files lists the config files that can name a slug.
func (s *Service) slugFiles(ctx context.Context) []string {
	files := []string{home.ConfigPath()}
	if s.ws == nil {
		return files
	}
	if list, err := s.ws.List(ctx); err == nil {
		for _, w := range list {
			files = append(files, filepath.Join(w.Path, "trebi.yaml"))
		}
	}
	if rows, err := s.ws.ListJobs(ctx, ""); err == nil {
		for _, r := range rows {
			files = append(files, r.Path)
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// apply rewrites every file that names old. It stops at the first failed
// write; the caller then calls rollback.
func (w *slugRewrite) apply(files []string) error {
	for _, path := range files {
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrSlugRewrite, path, err)
		}
		next, changed := w.rewrite(path, data)
		if !changed {
			continue
		}
		w.done = append(w.done, fileBackup{path: path, data: data, mode: st.Mode().Perm()})
		if err := writeFileMode(path, next, st.Mode().Perm()); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrSlugRewrite, path, err)
		}
	}
	return nil
}

// rollback writes the old content back to each changed file.
func (w *slugRewrite) rollback() {
	for _, b := range slices.Backward(w.done) {
		_ = writeFileMode(b.path, b.data, b.mode) // best effort; the caller reports the first error
	}
	w.done = nil
}

// changed lists the files that apply wrote.
func (w *slugRewrite) changed() []string {
	out := make([]string, 0, len(w.done))
	for _, b := range w.done {
		out = append(out, b.path)
	}
	return out
}

// rewrite returns data with each reference to old replaced. A markdown file
// is edited in its front matter only.
func (w *slugRewrite) rewrite(path string, data []byte) ([]byte, bool) {
	start, end := 0, len(data)
	if strings.HasSuffix(path, ".md") {
		fm, ok := frontMatter(data)
		if !ok {
			return data, false
		}
		start, end = fm[0], fm[1]
	}
	var doc yaml.Node
	if yaml.Unmarshal(data[start:end], &doc) != nil {
		return data, false
	}
	var hits []*yaml.Node
	w.collect(&doc, "", &hits)
	if len(hits) == 0 {
		return data, false
	}
	offsets := lineOffsets(data[start:end])
	out := slices.Clone(data)
	// Replace from the end, so earlier offsets stay valid.
	slices.SortFunc(hits, func(a, b *yaml.Node) int {
		if a.Line != b.Line {
			return b.Line - a.Line
		}
		return b.Column - a.Column
	})
	changed := false
	for _, n := range hits {
		if n.Line-1 >= len(offsets) {
			continue
		}
		at := start + offsets[n.Line-1] + n.Column - 1
		if at < len(out) && (out[at] == '"' || out[at] == '\'') {
			at++
		}
		if !bytes.HasPrefix(out[at:], []byte(w.old)) {
			continue
		}
		out = slices.Concat(out[:at], []byte(w.new), out[at+len(w.old):])
		changed = true
	}
	return out, changed
}

// collect finds the scalars that name old: items of a connectors: list, keys
// of a connectors: map, and event: and via: values.
func (w *slugRewrite) collect(n *yaml.Node, key string, hits *[]*yaml.Node) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			w.collect(c, key, hits)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if key == "connectors" && c.Kind == yaml.ScalarNode && c.Value == w.old {
				*hits = append(*hits, c)
				continue
			}
			w.collect(c, "", hits)
		}
	case yaml.MappingNode:
		w.collectMap(n, key, hits)
	}
}

// collectMap finds the keys of a connectors: map and the event: and via:
// values of one mapping.
func (w *slugRewrite) collectMap(n *yaml.Node, key string, hits *[]*yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if key == "connectors" && k.Value == w.old {
			*hits = append(*hits, k)
		}
		if (k.Value == "event" || k.Value == "via") && v.Kind == yaml.ScalarNode && v.Value == w.old {
			*hits = append(*hits, v)
			continue
		}
		w.collect(v, k.Value, hits)
	}
}

// frontMatter returns the byte range of the YAML between the leading ---
// lines of a markdown file.
func frontMatter(data []byte) ([2]int, bool) {
	first, rest, ok := bytes.Cut(data, []byte("\n"))
	if !ok || strings.TrimSpace(string(first)) != "---" {
		return [2]int{}, false
	}
	start := len(first) + 1
	off := start
	for len(rest) > 0 {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		if strings.TrimSpace(string(line)) == "---" {
			return [2]int{start, off}, true
		}
		off += len(line) + 1
		rest = tail
	}
	return [2]int{}, false
}

// lineOffsets returns the byte offset of each line start.
func lineOffsets(data []byte) []int {
	out := []int{0}
	for i, b := range data {
		if b == '\n' {
			out = append(out, i+1)
		}
	}
	return out
}

// writeFileMode writes a file through a temp file and a rename, and keeps
// the mode.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
