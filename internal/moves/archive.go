package moves

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Version is the format of a move archive: one tar stream with
// manifest.json first, then tables/<name>.jsonl, secrets.json,
// workspace/<rel> (the folder), and home/<rel> (files in the Trebi home).
const Version = "trebi-move/1"

// Manifest is manifest.json, the first entry of a move archive.
type Manifest struct {
	Version   string        `json:"version"`
	MoveID    string        `json:"move_id"`
	Workspace WorkspaceInfo `json:"workspace"`
	Source    MachineInfo   `json:"source"`
	Home      string        `json:"home"`
	Schema    int           `json:"schema_version"`
	Tables    []TableCount  `json:"tables"`
	Skipped   []Skipped     `json:"skipped"`
	Secrets   []string      `json:"secrets"`
	Files     []string      `json:"files"`
	CreatedAt time.Time     `json:"created_at"`
}

// WorkspaceInfo names the moved workspace on the source.
type WorkspaceInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Handle string `json:"handle"`
	Path   string `json:"path"`
}

// MachineInfo names a machine.
type MachineInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TableCount is the row count of one table in the archive.
type TableCount struct {
	Name string `json:"name"`
	Rows int    `json:"rows"`
}

// Skipped is a table that stays on the source, with the reason.
type Skipped struct {
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

// tableSpec selects the rows of one table that belong to a workspace. Each
// ? in where is the workspace id.
type tableSpec struct {
	name   string
	where  string
	drop   []string       // local row ids that the target makes again
	clear  map[string]any // values that are true only on the source machine
	ignore bool           // rows that other workspaces can share
}

const (
	inRuns     = `run_id IN (SELECT id FROM runs WHERE workspace_id = ?)`
	inConvs    = `conversation_id IN (SELECT id FROM conversations WHERE workspace_id = ?)`
	inArtifact = `artifact_id IN (SELECT id FROM artifacts WHERE workspace_id = ?)`
	inOwners   = `(owner_id = ? OR owner_id IN (SELECT id FROM job_index WHERE workspace_id = ?))`
	byWS       = `workspace_id = ?`
)

// tables are the moved tables, parents first. The workspaces row is apart.
var tables = []tableSpec{ //nolint:gochecknoglobals // read-only table list; the guard test checks it against the schema
	{name: "job_index", where: byWS},
	{name: "schedules", where: byWS},
	{name: "skill_index", where: byWS},
	{name: "conversations", where: byWS},
	{name: "conversation_messages", where: inConvs},
	{name: "conversation_events", where: inConvs},
	{name: "conversation_channels", where: inConvs},
	{name: "channel_pointers", where: byWS},
	{name: "assistant_sessions", where: byWS, clear: map[string]any{
		"status": "closed", "vendor_id": "", "actor_token_hash": "", "visor_session_id": nil, "visor_seq": 0,
	}},
	{name: "events", ignore: true, where: `id IN (SELECT trigger_event_id FROM runs WHERE workspace_id = ?
		UNION SELECT event_id FROM event_queue WHERE workspace_id = ?
		UNION SELECT event_id FROM event_outcomes WHERE workspace_id = ?
		UNION SELECT event_id FROM event_queue_batch WHERE queue_id IN (SELECT id FROM event_queue WHERE workspace_id = ?))`},
	{name: "runs", where: byWS, clear: map[string]any{
		"pane_session": nil, "runtime_pid": nil, "claimed_by": nil, "actor_token_hash": "", "visor_session_id": nil, "visor_seq": 0,
	}},
	{name: "run_turns", where: inRuns},
	{name: "run_events", where: inRuns, drop: []string{"id"}},
	{name: "run_segments", where: inRuns, drop: []string{"id"}},
	{name: "approvals", where: inRuns},
	{name: "event_queue", where: byWS, clear: map[string]any{"claimed_at": nil}},
	{name: "event_queue_batch", where: `queue_id IN (SELECT id FROM event_queue WHERE workspace_id = ?)`},
	{name: "event_outcomes", where: byWS},
	{name: "inbox_items", where: byWS},
	{name: "requests", where: byWS, clear: map[string]any{"claimed_at": nil}},
	{name: "request_forms", where: `request_id IN (SELECT id FROM requests WHERE workspace_id = ?)`},
	{name: "decisions", where: byWS},
	{name: "route_choices", where: byWS},
	{name: "chains", where: byWS},
	{name: "chain_hops", where: byWS, clear: map[string]any{"claimed_at": nil}},
	{name: "agent_proposals", where: byWS},
	{name: "artifacts", where: byWS},
	{name: "artifact_versions", where: inArtifact},
	{name: "artifact_snapshots", where: inArtifact},
	{name: "uploads", where: byWS},
	{name: "outbox", where: byWS, clear: map[string]any{"claimed_at": nil}},
	{name: "email_addresses", where: byWS},
	{name: "email_messages", where: byWS},
	{name: "cloud_addresses", where: byWS},
	{name: "cloud_hooks", where: byWS},
	{name: "state_rows", where: inOwners},
	{name: "state_run_stats", where: inOwners},
}

// skipped are the tables that stay on the source.
var skipped = []Skipped{ //nolint:gochecknoglobals // read-only list, written into each manifest
	{"event_log", "the bus history of the source machine"},
	{"idempotency_keys", "request keys of the source machine"},
	{"channel_threads", "connections are per machine"},
	{"connection_event_counts", "connections are per machine"},
	{"connection_hooks", "connections are per machine"},
	{"connection_stats", "connections are per machine"},
	{"connector_installs", "connector programs are per machine"},
	{"connector_package_holds", "connector programs are per machine"},
	{"webhook_deliveries", "connections are per machine"},
	{"harness_files", "harness setup is per machine"},
	{"harness_state", "harness setup is per machine"},
	{"harness_usage", "harness setup is per machine"},
	{"pauses", "pauses hold the runs of one machine"},
	{"pause_misses", "pauses hold the runs of one machine"},
	{"machines", "the machine list of the source"},
	{"machine_links", "links are per machine"},
	{"mobile_clients", "phones pair with one machine"},
	{"cloud_link", "the cloud link of the source machine"},
	{"cloud_cursors", "the cloud link of the source machine"},
	{"system_flags", "store bookkeeping"},
	{"schema_migrations", "store bookkeeping"},
	{"sqlite_sequence", "store bookkeeping"},
	{"v38_log_merge", "store bookkeeping"},
	{"workspace_moves", "move state of the source machine"},
	{"state_fts", "the target makes the search index again"},
}

// homeDirs are the folders of the Trebi home that an archive can write,
// relative to the home.
func homeDirs() []string {
	rel := func(p string) string {
		r, err := filepath.Rel(home.Base(), p)
		if err != nil {
			return p
		}
		return filepath.ToSlash(r)
	}
	return []string{rel(home.RunsLogDir()), rel(home.RunsDir()), rel(home.RequestsDir()), rel(home.UploadsDir()), rel(home.ArtifactBlobsDir())}
}

var secretRefRE = regexp.MustCompile(`secret:([A-Za-z0-9_][A-Za-z0-9_.@-]{0,127})`)

const scanMaxBytes = 4 << 20

// packer writes the archive of one workspace.
type packer struct {
	st      *store.Store
	home    string
	secrets secrets.Dir
}

type dumped struct {
	name string
	data []byte
	rows []map[string]any
}

// pack writes the archive to w. sent runs once the manifest and the tables
// are in the stream.
func (p packer) pack(ctx context.Context, w io.Writer, m Manifest, sent func()) (Manifest, error) { //nolint:gocyclo // one pass writes the archive parts in their fixed order
	dumps, err := p.dump(ctx, m.Workspace.ID)
	if err != nil {
		return m, err
	}
	m.Tables = nil
	for _, d := range dumps {
		m.Tables = append(m.Tables, TableCount{Name: d.name, Rows: len(d.rows)})
	}
	m.Skipped = skipped
	m.Schema = store.SchemaVersion
	m.Home = p.home
	m.Version = Version

	files, err := p.files(m.Workspace.Path, dumps)
	if err != nil {
		return m, err
	}
	vals, err := p.secretValues(m.Workspace)
	if err != nil {
		return m, err
	}
	m.Secrets = slices.Sorted(func(yield func(string) bool) {
		for k := range vals {
			if !yield(k) {
				return
			}
		}
	})
	m.Files = m.Files[:0]
	for _, f := range files {
		m.Files = append(m.Files, f.name)
	}

	tw := tar.NewWriter(w)
	if err := writeJSON(tw, "manifest.json", m); err != nil {
		return m, err
	}
	for _, d := range dumps {
		if err := writeBytes(tw, "tables/"+d.name+".jsonl", d.data); err != nil {
			return m, err
		}
	}
	if err := writeJSON(tw, "secrets.json", vals); err != nil {
		return m, err
	}
	if sent != nil {
		sent()
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		if err := f.write(tw); err != nil {
			return m, err
		}
	}
	return m, tw.Close()
}

// dump reads the workspace rows in one read transaction, so the tables
// agree with each other.
func (p packer) dump(ctx context.Context, wsID string) ([]dumped, error) {
	tx, err := p.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	specs := append([]tableSpec{{name: "workspaces", where: `id = ?`}}, tables...)
	out := make([]dumped, 0, len(specs))
	for _, sp := range specs {
		rows, err := selectRows(ctx, tx, sp, wsID)
		if err != nil {
			return nil, fmt.Errorf("dump %s: %w", sp.name, err)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				return nil, err
			}
		}
		out = append(out, dumped{name: sp.name, data: buf.Bytes(), rows: rows})
	}
	return out, nil
}

func selectRows(ctx context.Context, tx *sql.Tx, sp tableSpec, wsID string) ([]map[string]any, error) {
	args := make([]any, strings.Count(sp.where, "?"))
	for i := range args {
		args[i] = wsID
	}
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+sp.name+` WHERE `+sp.where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
				continue
			}
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// entry is one file or folder of the archive.
type entry struct {
	name string // archive name
	path string // local path
	info fs.FileInfo
}

func (e entry) write(tw *tar.Writer) error {
	link := ""
	if e.info.Mode()&fs.ModeSymlink != 0 {
		l, err := os.Readlink(e.path)
		if err != nil {
			return err
		}
		link = l
	}
	h, err := tar.FileInfoHeader(e.info, link)
	if err != nil {
		return err
	}
	h.Name = e.name
	h.Uname, h.Gname = "", ""
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if !e.info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(e.path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyN(tw, f, e.info.Size())
	return err
}

// files lists the workspace folder and the home files of the dumped rows.
func (p packer) files(wsPath string, dumps []dumped) ([]entry, error) { //nolint:gocyclo,funlen // one walk lists the folder and the home files of each table
	var out []entry
	err := filepath.WalkDir(wsPath, func(pth string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if pth == wsPath {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(wsPath, pth)
		if err != nil {
			return err
		}
		out = append(out, entry{name: "workspace/" + filepath.ToSlash(rel), path: pth, info: info})
		return nil
	})
	if err != nil {
		return nil, err
	}
	add := func(rel string) error {
		local := filepath.Join(p.home, filepath.FromSlash(rel))
		return filepath.WalkDir(local, func(pth string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			r, err := filepath.Rel(p.home, pth)
			if err != nil {
				return err
			}
			out = append(out, entry{name: "home/" + filepath.ToSlash(r), path: pth, info: info})
			return nil
		})
	}
	dirs := homeDirs()
	logs, runDirs, reqs, uploads, blobs := dirs[0], dirs[1], dirs[2], dirs[3], dirs[4]
	ws := dumps[0].rows
	if len(ws) == 1 {
		if id, _ := ws[0]["id"].(string); id != "" {
			if err := add(path.Join(uploads, id)); err != nil {
				return nil, err
			}
		}
	}
	shas := map[string]bool{}
	for _, d := range dumps {
		for _, r := range d.rows {
			var err error
			switch d.name {
			case "runs":
				id, _ := r["id"].(string)
				if id == "" {
					continue
				}
				if err = add(path.Join(logs, id+".log")); err == nil {
					err = add(path.Join(runDirs, id))
				}
			case "requests":
				if id, _ := r["id"].(string); id != "" {
					err = add(path.Join(reqs, id))
				}
			case "artifact_versions":
				shas[str(r["sha256"])] = true
			case "artifact_snapshots":
				shas[str(r["data_sha256"])] = true
			case "decisions":
				shas[str(r["raw_sha256"])] = true
			}
			if err != nil {
				return nil, err
			}
		}
	}
	for _, sha := range slices.Sorted(func(yield func(string) bool) {
		for k := range shas {
			if !yield(k) {
				return
			}
		}
	}) {
		if len(sha) < 3 || strings.ContainsAny(sha, `/\.`) {
			continue
		}
		if err := add(path.Join(blobs, sha[:2], sha)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// secretValues reads the secrets that move: the connector secrets of the
// workspace (name@<workspace id>) and each secret:<name> that a file of the
// workspace names. A missing or reserved name stays behind.
func (p packer) secretValues(ws WorkspaceInfo) (map[string]string, error) { //nolint:gocyclo // one scan collects the workspace secrets and the referenced secrets
	names := map[string]bool{}
	entries, err := os.ReadDir(string(p.secrets))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "@"+ws.ID) {
			names[e.Name()] = true
		}
	}
	err = filepath.WalkDir(ws.Path, func(pth string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if pth != ws.Path && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > scanMaxBytes {
			return err
		}
		data, err := os.ReadFile(pth)
		if err != nil {
			return err
		}
		for _, m := range secretRefRE.FindAllSubmatch(data, -1) {
			names[strings.TrimRight(string(m[1]), ".-")] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n := range names {
		if secrets.Reserved(n) || !p.secrets.Has(n) {
			continue
		}
		v, err := p.secrets.Get(n)
		if err != nil {
			return nil, err
		}
		out[n] = string(v)
	}
	return out, nil
}

func writeJSON(tw *tar.Writer, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeBytes(tw, name, b)
}

func writeBytes(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// unpacker restores an archive on the target machine.
type unpacker struct {
	st      *store.Store
	ws      *workspaces.Service
	home    string
	secrets secrets.Dir

	dest     string   // new workspace folder
	created  []string // home files and folders that this restore made
	secretsN []string // secrets that this restore made
	links    []*tar.Header
}

// ImportResult is the reply of POST /v1/workspaces/import.
type ImportResult struct {
	WorkspaceID  string       `json:"workspace_id"`
	Path         string       `json:"path"`
	Already      bool         `json:"already,omitempty"`
	Tables       []TableCount `json:"tables"`
	Files        int          `json:"files"`
	SecretsAdded []string     `json:"secrets_added"`
	SecretsKept  []string     `json:"secrets_kept"`
}

// Errors of a restore.
var (
	ErrExists   = errors.New("a workspace with this id is on this machine")
	ErrVersion  = errors.New("the move archive is not valid")
	ErrNewer    = errors.New("the other machine runs a newer Trebi; update Trebi on this machine first")
	ErrNoFolder = errors.New("set a folder for new workspaces on this machine first")
)

func (u *unpacker) restore(ctx context.Context, r io.Reader, failAt string) (res *ImportResult, err error) { //nolint:gocyclo,funlen // one read of the tar in its fixed order; undo covers every step
	tr := tar.NewReader(bufio.NewReaderSize(r, 1<<16))
	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVersion, err)
	}
	if h.Name != "manifest.json" {
		return nil, fmt.Errorf("%w: manifest.json is not first", ErrVersion)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVersion, err)
	}
	if m.Version != Version || m.Workspace.ID == "" || m.MoveID == "" || !filepath.IsLocal(filepath.Base(m.Workspace.Path)) {
		return nil, fmt.Errorf("%w: version %q", ErrVersion, m.Version)
	}
	if m.Schema > store.SchemaVersion {
		return nil, ErrNewer
	}
	var moveID, have string
	switch err := u.st.DB().QueryRowContext(ctx, `SELECT COALESCE(move_id, ''), path FROM workspaces WHERE id = ?`, m.Workspace.ID).Scan(&moveID, &have); {
	case err == nil && moveID == m.MoveID:
		_, _ = io.Copy(io.Discard, r)
		return &ImportResult{WorkspaceID: m.Workspace.ID, Path: have, Already: true}, nil
	case err == nil:
		return nil, ErrExists
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if err := u.pickFolder(filepath.Base(m.Workspace.Path)); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			u.undo()
		}
	}()

	rows := map[string][]map[string]any{}
	var vals map[string]string
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(h.Name, "tables/"):
			name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "tables/"), ".jsonl")
			list, err := readRows(tr)
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", name, err)
			}
			rows[name] = list
		case h.Name == "secrets.json":
			if err := json.NewDecoder(tr).Decode(&vals); err != nil {
				return nil, fmt.Errorf("%w: secrets: %v", ErrVersion, err)
			}
		case strings.HasPrefix(h.Name, "workspace/"):
			if err := u.writeFile(tr, h, u.dest, strings.TrimPrefix(h.Name, "workspace/"), false); err != nil {
				return nil, err
			}
			files++
		case strings.HasPrefix(h.Name, "home/"):
			if err := u.writeHome(tr, h); err != nil {
				return nil, err
			}
			files++
		}
	}
	if err := u.makeLinks(); err != nil {
		return nil, err
	}
	if len(rows["workspaces"]) != 1 {
		return nil, fmt.Errorf("%w: no workspace row", ErrVersion)
	}
	if _, err := workspaces.RewriteTree(u.dest, m.Workspace.Path, u.dest); err != nil {
		return nil, err
	}
	res = &ImportResult{WorkspaceID: m.Workspace.ID, Path: u.dest, Files: files, SecretsAdded: []string{}, SecretsKept: []string{}}
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range vals {
			if !yield(k) {
				return
			}
		}
	}) {
		if secrets.CheckName(n) != nil || secrets.Reserved(n) {
			continue
		}
		if u.secrets.Has(n) {
			res.SecretsKept = append(res.SecretsKept, n)
			continue
		}
		if err := u.secrets.Set(n, vals[n]); err != nil {
			return nil, err
		}
		u.secretsN = append(u.secretsN, n)
		res.SecretsAdded = append(res.SecretsAdded, n)
	}
	if failAt == "rows" {
		return nil, errors.New("injected restore fault")
	}
	rw := rewriter{{m.Workspace.Path, u.dest}, {m.Home, u.home}}
	ws := rows["workspaces"][0]
	if err := u.insert(ctx, m, ws, rows, rw, res); err != nil {
		return nil, err
	}
	if w, gerr := u.ws.Get(ctx, m.Workspace.ID); gerr == nil {
		_ = u.ws.RebuildIndex(ctx, w) // the rows are in; a parse fault shows on the job
	}
	return res, nil
}

func (u *unpacker) insert(ctx context.Context, m Manifest, ws map[string]any, rows map[string][]map[string]any, rw rewriter, res *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	rev, _ := num(ws["revision"]).(int64)
	_, err := u.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		var self string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE is_self = 1`).Scan(&self); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		spec := tableSpec{name: "workspaces", clear: map[string]any{
			"path": u.dest, "machine_id": store.NullStr(self), "move_id": m.MoveID, "orphaned": 0,
			"archived_at": nil, "moved_to": nil, "updated_at": now,
		}}
		if _, err := insertRows(ctx, tx, spec, []map[string]any{ws}, rw); err != nil {
			return err
		}
		for _, sp := range tables {
			n, err := insertRows(ctx, tx, sp, rows[sp.name], rw)
			if err != nil {
				return fmt.Errorf("restore %s: %w", sp.name, err)
			}
			res.Tables = append(res.Tables, TableCount{Name: sp.name, Rows: n})
		}
		return nil
	}, store.BusEvent{
		Topic: "workspace.created", Entity: "workspace", EntityID: m.Workspace.ID, Action: "created", Revision: rev,
		Attrs: map[string]string{"path": u.dest, "name": m.Workspace.Name, "workspace": m.Workspace.ID, "moved_from": m.Source.Name},
	})
	return err
}

// insertRows writes rows into the columns that the target table has.
func insertRows(ctx context.Context, tx *sql.Tx, sp tableSpec, rows []map[string]any, rw rewriter) (int, error) { //nolint:gocyclo // one loop applies the clear, drop, and rewrite rules of a row
	if len(rows) == 0 {
		return 0, nil
	}
	cols, err := tableColumns(ctx, tx, sp.name)
	if err != nil {
		return 0, err
	}
	verb := "INSERT"
	if sp.ignore {
		verb = "INSERT OR IGNORE"
	}
	n := 0
	for _, r := range rows {
		var names []string
		for c := range r {
			if cols[c] && !slices.Contains(sp.drop, c) {
				names = append(names, c)
			}
		}
		for c := range sp.clear {
			if cols[c] && !slices.Contains(names, c) {
				names = append(names, c)
			}
		}
		slices.Sort(names)
		args := make([]any, len(names))
		for i, c := range names {
			v, ok := sp.clear[c]
			if !ok {
				v = r[c]
			}
			args[i] = rw.value(num(v))
		}
		q := verb + ` INTO ` + sp.name + ` (` + strings.Join(names, ", ") + `) VALUES (` + strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + `)`
		out, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return n, err
		}
		if k, _ := out.RowsAffected(); k == 0 {
			continue
		}
		n++
		if sp.name == "state_rows" {
			id, err := out.LastInsertId()
			if err != nil {
				return n, err
			}
			key, value := str(r["key"]), str(r["value"])
			if _, err := tx.ExecContext(ctx, `INSERT INTO state_fts (rowid, key, body) VALUES (?, ?, ?)`, id, key, state.FTSBody(value)); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table+` LIMIT 0`)
	if err != nil {
		return nil, fmt.Errorf("table %s is not on this machine: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(cols))
	for _, c := range cols {
		out[c] = true
	}
	return out, nil
}

func readRows(r io.Reader) ([]map[string]any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var out []map[string]any
	for {
		var row map[string]any
		err := dec.Decode(&row)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
}

// num turns a JSON number into an int64 or a float64.
func num(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return f
}

// rewriter replaces the source paths in a row value with the target paths.
type rewriter [][2]string

func (rw rewriter) value(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	for _, p := range rw {
		if p[0] != "" {
			s, _ = workspaces.RewritePathPrefix(s, p[0], p[1])
		}
	}
	return s
}

// pickFolder chooses the new folder under workspaces.root, with a number
// suffix when the name is taken.
func (u *unpacker) pickFolder(base string) error {
	root := u.ws.Root()
	if root == "" {
		return ErrNoFolder
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		dest := filepath.Join(root, name)
		if err := os.Mkdir(dest, 0o755); err == nil {
			u.dest = dest
			return nil
		} else if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("no free folder name for %s in %s", base, root)
}

// writeFile writes one archive entry under root. Links wait until all files
// are written, so that no file goes through a link.
func (u *unpacker) writeFile(r io.Reader, h *tar.Header, root, rel string, track bool) error {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return fmt.Errorf("%w: path %q", ErrVersion, h.Name)
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	switch h.Typeflag {
	case tar.TypeDir:
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		if err := u.mkdirAll(dst, track); err != nil {
			return err
		}
		return os.Chmod(dst, fs.FileMode(h.Mode).Perm()|0o700)
	case tar.TypeSymlink:
		hh := *h
		hh.Name = dst
		u.links = append(u.links, &hh)
		return nil
	case tar.TypeReg:
	default:
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		if track {
			return nil // a home file that is here already, such as a blob
		}
		return fmt.Errorf("%w: %s twice", ErrVersion, rel)
	}
	if err := u.mkdirAll(filepath.Dir(dst), track); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(h.Mode).Perm())
	if err != nil {
		return err
	}
	if track {
		u.created = append(u.created, dst)
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chtimes(dst, h.ModTime, h.ModTime)
}

func (u *unpacker) writeHome(r io.Reader, h *tar.Header) error {
	rel := strings.TrimPrefix(h.Name, "home/")
	ok := false
	for _, d := range homeDirs() {
		if strings.HasPrefix(rel, d+"/") {
			ok = true
			break
		}
	}
	if !ok || h.Typeflag == tar.TypeSymlink {
		return fmt.Errorf("%w: home path %q", ErrVersion, rel)
	}
	return u.writeFile(r, h, u.home, rel, true)
}

func (u *unpacker) mkdirAll(dir string, track bool) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		missing = append(missing, d)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if track {
			u.created = append(u.created, missing[i])
		}
	}
	return nil
}

func (u *unpacker) makeLinks() error {
	for _, h := range u.links {
		if err := u.mkdirAll(filepath.Dir(h.Name), false); err != nil {
			return err
		}
		if err := os.Symlink(h.Linkname, h.Name); err != nil {
			return err
		}
	}
	return nil
}

// undo removes what a failed restore wrote.
func (u *unpacker) undo() {
	if u.dest != "" {
		_ = os.RemoveAll(u.dest)
	}
	for i := len(u.created) - 1; i >= 0; i-- {
		_ = os.Remove(u.created[i])
	}
	for _, n := range u.secretsN {
		_ = u.secrets.Delete(n)
	}
}
