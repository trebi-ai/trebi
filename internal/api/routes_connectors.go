package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/registry"
	"github.com/trebi-ai/trebi/internal/secrets"
)

//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeConnectors(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connectors", s.handleConnectors)
	mux.HandleFunc("POST /v1/connectors", s.handleConnectorsCreate)
	mux.HandleFunc("GET /v1/connectors/catalog", s.handleConnectorsCatalog)
	mux.HandleFunc("GET /v1/connectors/catalog/entry", s.handleConnectorCatalogEntry)
	mux.HandleFunc("POST /v1/connectors/install", s.handleConnectorInstall)
	mux.HandleFunc("GET /v1/connectors/{name}", s.handleConnectorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}", s.handleConnectorPut)
	mux.HandleFunc("PATCH /v1/connectors/{name}", s.handleConnectorPatch)
	mux.HandleFunc("DELETE /v1/connectors/{name}", s.handleConnectorDelete)
	mux.HandleFunc("POST /v1/connectors/{name}/connect", s.handleConnectorStart)
	mux.HandleFunc("POST /v1/connectors/{name}/disconnect", s.handleConnectorStop)
	mux.HandleFunc("POST /v1/connectors/{name}/restart", s.handleConnectorRestart)
	mux.HandleFunc("POST /v1/connectors/{name}/test", s.handleConnectorTest)
	mux.HandleFunc("GET /v1/connectors/{name}/logs", s.handleConnectorLogs)
	mux.HandleFunc("GET /v1/connectors/{name}/cursor", s.handleConnectorCursorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}/cursor", s.handleConnectorCursorPut)
}

func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request) {
	capability := r.URL.Query().Get("capability")
	if capability != "" && capability != "actions" && capability != "events" && capability != "channel" {
		writeErr(w, http.StatusBadRequest, "validation", "capability must be actions, events, or channel")
		return
	}
	provenance := r.URL.Query().Get("provenance")
	if provenance != "" && provenance != "managed" && provenance != "inherited" && provenance != "native" {
		writeErr(w, http.StatusBadRequest, "validation", "provenance must be managed, inherited, or native")
		return
	}
	_ = s.connectors.Load() // refresh from disk; a failed load shows as load_error
	list := s.connectors.List()
	if row := s.email.ConnectorRow(); row != nil {
		list = append(list, row) // the native email channel rides the list
	}
	if row := s.hooks.ConnectorRow(); row != nil {
		list = append(list, row) // the native webhook connector rides the list
	}
	if capability != "" || provenance != "" {
		filtered := list[:0]
		for _, row := range list {
			if !rowHasCapability(row, capability) || !rowHasProvenance(row, provenance) {
				continue
			}
			filtered = append(filtered, row)
		}
		list = filtered
	}
	sort.Slice(list, func(i, j int) bool {
		return fmt.Sprint(list[i]["name"]) < fmt.Sprint(list[j]["name"])
	})
	// Name-keyed collection: limit only, no ULID cursor.
	limit := parseLimit(r)
	truncated := len(list) > limit
	if truncated {
		list = list[:limit]
	}
	resp := map[string]any{"connectors": list}
	if truncated {
		resp["truncated"] = true
	}
	if err := s.connectors.LoadErr(); err != nil {
		resp["load_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// rowHasCapability reports whether one row carries a capability. An empty
// want matches every row.
func rowHasCapability(row map[string]any, want string) bool {
	if want == "" {
		return true
	}
	caps, ok := row["capabilities"].([]string)
	if !ok {
		if anyCaps, ok := row["capabilities"].([]any); ok {
			for _, c := range anyCaps {
				if fmt.Sprint(c) == want {
					return true
				}
			}
		}
		return false
	}
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func rowHasProvenance(row map[string]any, want string) bool {
	if want == "" {
		return true
	}
	return fmt.Sprint(row["provenance"]) == want
}

func (s *Server) handleConnectorsCreate(w http.ResponseWriter, r *http.Request) {
	var d connectors.Definition
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if d.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	if err := s.connectors.Create(r.Context(), d); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(d.Name)
	if err != nil {
		writeJSON(w, http.StatusCreated, d)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleConnectorGet(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("name") {
	case "email":
		if row := s.email.ConnectorRow(); row != nil {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "email is not configured")
		return
	case "webhook":
		if row := s.hooks.ConnectorRow(); row != nil {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "webhook is not available")
		return
	}
	row, err := s.connectors.Get(r.PathValue("name"))
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorPut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var d connectors.Definition
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	d.Name = r.PathValue("name")
	if err := s.connectors.Replace(r.Context(), d); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(d.Name)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorPatch(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var p connectors.Patch
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	name := r.PathValue("name")
	if err := s.connectors.Patch(r.Context(), name, p); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(name)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	if err := s.connectors.Delete(r.Context(), r.PathValue("name")); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

// requireEventsBlock refuses lifecycle and stream routes on a connector with
// no events block. Native connectors carry events in their own service.
func (s *Server) requireEventsBlock(name string) error {
	if name == "email" || name == "webhook" {
		return nil
	}
	d, ok := s.connectors.Definition(name)
	if !ok {
		if _, err := s.connectors.Get(name); err != nil {
			return err
		}
		return fmt.Errorf("%w: connector %q is not managed", connectors.ErrInherited, name)
	}
	if d.Events == nil {
		return fmt.Errorf("%w: %q", connectors.ErrNoEvents, name)
	}
	return nil
}

func (s *Server) handleConnectorStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.connectors.Start(r.Context(), name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.connectors.Stop(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped"})
}

// handleConnectorRestart restarts a managed connector. The email row's
// restart is a forced poll and nothing else: the native channel has no
// process (plan K).
func (s *Server) handleConnectorRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "email" {
		s.handleEmailPoll(w, r)
		return
	}
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := s.connectors.Restart(r.Context(), name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var body struct {
		Event any `json:"event"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Event == nil {
		body.Event = map[string]any{"test": true}
	}
	_, err := s.connectors.InjectEvent(r.Context(), name, body.Event)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleConnectorLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// The native channel has no process, so it has no process log. Its
	// activity is the email log (plan K).
	if err := s.connectors.GuardReserved(name); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	path := filepath.Join(home.ConnectorsLogDir(), name+".jsonl")
	flusher, ok := w.(http.Flusher)
	if !ok {
		data, _ := os.ReadFile(path)
		writeJSON(w, http.StatusOK, map[string]any{"log": string(data)})
		return
	}
	// Headers must go out before the first log line, or an empty file + follow
	// holds the connection with no response and proxies time out as 502.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	var offset int64
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fmt.Fprintf(w, "data: %s\n\n", sc.Text())
			flusher.Flush()
		}
		offset, _ = f.Seek(0, io.SeekCurrent)
		_ = f.Close()
	}

	if r.URL.Query().Get("follow") != "1" {
		return
	}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			f2, err := os.Open(path)
			if err != nil {
				continue
			}
			st, err := f2.Stat()
			if err != nil || st.Size() <= offset {
				_ = f2.Close()
				continue
			}
			_, _ = f2.Seek(offset, io.SeekStart)
			sc2 := bufio.NewScanner(f2)
			for sc2.Scan() {
				fmt.Fprintf(w, "data: %s\n\n", sc2.Text())
				flusher.Flush()
			}
			offset, _ = f2.Seek(0, io.SeekCurrent)
			_ = f2.Close()
		}
	}
}

func (s *Server) handleConnectorCursorGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	cur, err := s.connectors.GetCursor(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var updated any
	if cur.UpdatedAt != "" {
		updated = cur.UpdatedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": cur.LastEventID, "updated_at": updated})
}

func (s *Server) handleConnectorCursorPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.requireEventsBlock(name); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var body struct {
		LastEventID string `json:"last_event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body sets last_event_id to ""
	if err := s.connectors.PutCursor(r.Context(), name, body.LastEventID); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "last_event_id": body.LastEventID})
}

func (s *Server) writeConnectorErr(w http.ResponseWriter, err error) bool {
	var amb *connectors.AmbiguousError
	var ve *connectors.ValidationError
	switch {
	case errors.As(err, &amb):
		writeErr(w, http.StatusBadRequest, "ambiguous", err.Error())
	case errors.As(err, &ve):
		details := map[string]any{"fields": ve.Fields}
		writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), details)
	case errors.Is(err, connectors.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, connectors.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, connectors.ErrExists),
		errors.Is(err, connectors.ErrLoadFailed):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrNoEvents):
		writeErr(w, http.StatusConflict, "validation", err.Error())
	case errors.Is(err, connectors.ErrInherited):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrReserved):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		return false
	}
	return true
}

// handleConnectorsCatalog searches the configured MCP registries. With no
// registry it merges the pages of every registry in order.
func (s *Server) handleConnectorsCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, next, err := s.catalogRegistry().Search(r.Context(), registry.SearchOpts{
		Query:    q.Get("q"),
		Registry: q.Get("registry"),
		Cursor:   q.Get("cursor"),
		Limit:    parseLimit(r),
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	list := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		list = append(list, catalogEntryRow(e))
	}
	resp := map[string]any{"entries": list}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConnectorCatalogEntry reads one server entry and its env variables.
func (s *Server) handleConnectorCatalogEntry(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	entry, err := s.catalogRegistry().Get(r.Context(), q.Get("registry"), name, q.Get("version"))
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	row := catalogEntryRow(entry)
	row["env"] = catalogEnvFlags(entry.Server)
	writeJSON(w, http.StatusOK, row)
}

// handleConnectorInstall installs one registry entry as a connector. It
// writes the entry secrets first, then creates the connector; a create
// failure restores the secrets.
func (s *Server) handleConnectorInstall(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Registry string            `json:"registry"`
		Name     string            `json:"name"`
		Version  string            `json:"version"`
		As       string            `json:"as"`
		Package  *int              `json:"package"`
		Remote   *int              `json:"remote"`
		Env      map[string]string `json:"env"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	entry, err := s.catalogRegistry().Get(r.Context(), body.Registry, body.Name, body.Version)
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	def, secretWrites, err := entry.Definition(registry.InstallOpts{
		Name:    body.As,
		Package: indexOr(body.Package),
		Remote:  indexOr(body.Remote),
		Env:     body.Env,
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	restore, err := writeRegistrySecrets(secretWrites)
	if err != nil {
		restore()
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.connectors.Create(r.Context(), def); err != nil {
		restore()
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(def.Name)
	if err != nil {
		row = map[string]any{"name": def.Name}
	}
	if sk, ok := entry.Skill(); ok {
		// The entry contributes a skill (Part H.4). Fetch its body, write it
		// with connector provenance, and point the CLI action at it.
		md, ferr := fetchRegistrySkill(r.Context(), sk.URL)
		if ferr != nil {
			s.connectors.Delete(r.Context(), def.Name)
			restore()
			writeErr(w, http.StatusBadGateway, "internal", ferr.Error())
			return
		}
		written, serr := s.skills.AddConnectorSkill(r.Context(), sk.Name, md, def.Name)
		if serr != nil {
			s.connectors.Delete(r.Context(), def.Name)
			restore()
			writeErr(w, http.StatusInternalServerError, "internal", serr.Error())
			return
		}
		row["skill"] = map[string]any{"name": written.Name, "from_connector": def.Name}
	}
	writeJSON(w, http.StatusCreated, row)
}

// fetchRegistrySkill reads the SKILL.md body of a contributed skill. It caps
// the body so a bad URL cannot exhaust memory.
func fetchRegistrySkill(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("skill url %q is invalid", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("skill url %s: status %d", u.String(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// catalogEntryRow is the search/list view of one registry entry.
func catalogEntryRow(e registry.Entry) map[string]any {
	row := map[string]any{
		"registry": e.Registry,
		"name":     e.Server.Name,
		"version":  e.Server.Version,
	}
	if e.Server.Title != "" {
		row["title"] = e.Server.Title
	}
	if e.Server.Description != "" {
		row["description"] = e.Server.Description
	}
	row["packages"] = e.Server.Packages
	row["remotes"] = e.Server.Remotes
	return row
}

// catalogEnvFlags lists the declared env variables with their required and
// secret flags, deduplicated across packages.
func catalogEnvFlags(s registry.ServerJSON) []map[string]any {
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, p := range s.Packages {
		for _, v := range p.EnvironmentVariables {
			if v.Name == "" || seen[v.Name] {
				continue
			}
			seen[v.Name] = true
			out = append(out, map[string]any{
				"name":        v.Name,
				"description": v.Description,
				"format":      v.Format,
				"required":    v.IsRequired,
				"secret":      v.IsSecret,
			})
		}
	}
	return out
}

// indexOr maps an optional JSON index to the -1 sentinel InstallOpts uses.
func indexOr(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

// writeRegistryErr maps a registry client error to a status code.
func (s *Server) writeRegistryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrUnknownRegistry):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, registry.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, registry.ErrUnsupportedPackage),
		errors.Is(err, registry.ErrUnsupportedRemote),
		errors.Is(err, registry.ErrMissingEnv):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
	}
}

// writeRegistrySecrets stores each secret and returns a restore function. The
// restore puts back a prior value, or deletes a secret that did not exist, so
// a failed install leaves the store as it was.
func writeRegistrySecrets(writes []registry.SecretWrite) (func(), error) {
	type prior struct {
		value   string
		existed bool
	}
	prev := make([]prior, 0, len(writes))
	written := 0
	restore := func() {
		for i := written - 1; i >= 0; i-- {
			if prev[i].existed {
				_ = secrets.Set(writes[i].Name, prev[i].value)
				continue
			}
			_ = secrets.Delete(writes[i].Name)
		}
	}
	for i, w := range writes {
		p := prior{}
		if cur, err := secrets.Get(w.Name); err == nil {
			p.value, p.existed = string(cur), true
		}
		prev = append(prev, p)
		if err := secrets.Set(w.Name, w.Value); err != nil {
			written = i
			return restore, err
		}
		written = i + 1
	}
	return restore, nil
}

// catalogRegistry returns the registry client for the current config. A
// change to connectors.registries takes effect on the next read, so a local
// registry can start after the daemon does (plan H.1).
func (s *Server) catalogRegistry() *registry.Service {
	if s.catalogRegs == nil {
		return s.registry // a test override, or a config that failed to load
	}
	s.catalogRegMu.Lock()
	defer s.catalogRegMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		return s.registry
	}
	regs := cfg.Connectors.Registries
	if sameRegistries(regs, s.catalogRegs) {
		return s.registry
	}
	s.catalogRegs = regs
	s.registry = registry.New(registry.Config{Registries: regs, Log: s.log.With("source", "registry")})
	return s.registry
}

// sameRegistries reports whether two registry lists hold the same pairs.
func sameRegistries(a, b []config.Registry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
