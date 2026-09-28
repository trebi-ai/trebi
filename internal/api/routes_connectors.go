package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/registry"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
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
		if errors.Is(err, connectors.ErrDuplicate) {
			writeErr(w, http.StatusConflict, "conflict", err.Error()+"; change the id field of the test event")
			return
		}
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
// registry it merges the pages of every registry in order. The reply lists
// the configured registries, and each registry that failed while the others
// answered.
func (s *Server) handleConnectorsCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cat := s.catalogRegistry()
	res, err := cat.Search(r.Context(), registry.SearchOpts{
		Query:    q.Get("q"),
		Registry: q.Get("registry"),
		Cursor:   q.Get("cursor"),
		Limit:    parseLimit(r),
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	list := make([]map[string]any, 0, len(res.Entries))
	for _, e := range res.Entries {
		list = append(list, catalogEntryRow(e))
	}
	resp := map[string]any{"entries": list, "registries": cat.Registries}
	if res.Next != "" {
		resp["next_cursor"] = res.Next
	}
	if len(res.Errors) > 0 {
		resp["registry_errors"] = res.Errors
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConnectorCatalogEntry reads one server entry, its env variables, and
// the inputs of each package and remote.
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
	row["package_inputs"], row["remote_inputs"] = entry.Inputs()
	writeJSON(w, http.StatusOK, row)
}

// handleConnectorInstall installs one registry entry as a connector. Registry
// data is untrusted: it validates the definition and the skill name first,
// then checks the secrets, writes them, and creates the connector. A later
// failure removes the connector and restores the secrets.
func (s *Server) handleConnectorInstall(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Registry         string            `json:"registry"`
		Name             string            `json:"name"`
		Version          string            `json:"version"`
		As               string            `json:"as"`
		Package          *int              `json:"package"`
		Remote           *int              `json:"remote"`
		Env              map[string]string `json:"env"`
		OverwriteSecrets bool              `json:"overwrite_secrets"`
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
	if body.Package != nil && body.Remote != nil {
		writeErr(w, http.StatusBadRequest, "validation", "give package or remote, not both")
		return
	}
	cat := s.catalogRegistry()
	entry, err := cat.Get(r.Context(), body.Registry, body.Name, body.Version)
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
	sk, hasSkill := entry.Skill()
	if hasSkill {
		if err := skills.CheckName(sk.Name); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	sw, err := newSecretWrites(secretWrites, body.OverwriteSecrets)
	if err != nil {
		if errors.Is(err, errSecretConflict) {
			writeErr(w, http.StatusConflict, "conflict", err.Error()+"; send overwrite_secrets to replace it")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	def.Source.Secrets = sw.created()
	if err := def.Validate(); err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return
	}
	if err := sw.write(); err != nil {
		sw.restore(s.log)
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.connectors.Create(r.Context(), def); err != nil {
		sw.restore(s.log)
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
	if hasSkill {
		// The entry contributes a skill (Part H.4). Fetch its body, write it
		// with connector provenance, and point the CLI action at it.
		written, serr := s.installSkill(r.Context(), cat, sk, def.Name)
		if serr != nil {
			if derr := s.connectors.Delete(r.Context(), def.Name); derr != nil {
				s.log.Error("connector install rollback", "connector", def.Name, "err", derr)
			}
			sw.restore(s.log)
			switch {
			case errors.Is(serr, skills.ErrExists):
				writeErr(w, http.StatusConflict, "conflict", serr.Error())
			case errors.Is(serr, errSkillFetch):
				writeErr(w, http.StatusBadGateway, "upstream", serr.Error())
			default:
				writeErr(w, http.StatusInternalServerError, "internal", serr.Error())
			}
			return
		}
		row["skill"] = map[string]any{"name": written.Name, "from_connector": def.Name}
	}
	writeJSON(w, http.StatusCreated, row)
}

// errSkillFetch marks a contributed skill that could not be read.
var errSkillFetch = errors.New("fetch contributed skill")

// installSkill fetches a contributed skill and writes it for the connector.
func (s *Server) installSkill(ctx context.Context, cat *registry.Service, sk *registry.Skill, connector string) (*skills.Skill, error) {
	md, err := cat.FetchSkill(ctx, sk.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSkillFetch, err)
	}
	return s.skills.AddConnectorSkill(ctx, sk.Name, md, connector)
}

// catalogEntryRow is the search/list view of one registry entry.
func catalogEntryRow(e registry.Entry) map[string]any {
	row := map[string]any{
		"registry": e.Registry,
		"name":     e.Server.Name,
		"version":  e.Server.Version,
		"trust":    e.Trust,
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
		errors.Is(err, registry.ErrMissingInput),
		errors.Is(err, registry.ErrUnusedInput),
		errors.Is(err, registry.ErrInvalidInput),
		errors.Is(err, registry.ErrInvalidEntry),
		errors.Is(err, registry.ErrUnsafeURL):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
	}
}

// errSecretConflict is an install secret that exists with another value.
var errSecretConflict = errors.New("secret exists with a different value")

// secretWrites is the secret part of one install. It knows the prior value of
// each name, so a failed install can put the store back as it was.
type secretWrites struct {
	writes []registry.SecretWrite
	prior  []*string // nil: the secret did not exist
	done   int
}

// newSecretWrites reads the prior values. A secret that exists with the same
// value needs no write. One with a different value is errSecretConflict,
// unless overwrite is set.
func newSecretWrites(writes []registry.SecretWrite, overwrite bool) (*secretWrites, error) {
	sw := &secretWrites{}
	for _, w := range writes {
		cur, err := secrets.Get(w.Name)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			sw.writes = append(sw.writes, w)
			sw.prior = append(sw.prior, nil)
			continue
		}
		if string(cur) == w.Value {
			continue
		}
		if !overwrite {
			return nil, fmt.Errorf("%w: %s", errSecretConflict, w.Name)
		}
		v := string(cur)
		sw.writes = append(sw.writes, w)
		sw.prior = append(sw.prior, &v)
	}
	return sw, nil
}

// created lists the names that did not exist before the install. The
// connector owns them, and its delete removes them.
func (sw *secretWrites) created() []string {
	var out []string
	for i, w := range sw.writes {
		if sw.prior[i] == nil {
			out = append(out, w.Name)
		}
	}
	return out
}

// write stores each secret in order.
func (sw *secretWrites) write() error {
	for _, w := range sw.writes {
		if err := secrets.Set(w.Name, w.Value); err != nil {
			return err
		}
		sw.done++
	}
	return nil
}

// restore puts back each prior value, or deletes a secret that did not
// exist. It also covers a write that failed half way.
func (sw *secretWrites) restore(log *slog.Logger) {
	n := min(sw.done+1, len(sw.writes))
	for i := n - 1; i >= 0; i-- {
		name := sw.writes[i].Name
		var err error
		if p := sw.prior[i]; p != nil {
			err = secrets.Set(name, *p)
		} else {
			err = secrets.Delete(name)
		}
		if err != nil {
			log.Error("restore install secret", "secret", name, "err", err)
		}
	}
}

// catalogRegistry returns the registry client for the current config. A
// change to connectors.registries takes effect on the next read, so a local
// registry can start after the daemon does (plan H.1). The client is swapped
// atomically, so a request never reads a half-built client.
func (s *Server) catalogRegistry() *registry.Service {
	cur := s.catalog.Load()
	if s.catalogFixed {
		return cur
	}
	cfg, err := config.Load()
	if err != nil {
		return cur // keep the last good client
	}
	regs := cfg.Connectors.Registries
	if len(regs) == 0 {
		regs = config.DefaultRegistries()
	}
	if slices.Equal(regs, cur.Registries) {
		return cur
	}
	next := registry.New(registry.Config{Registries: regs, Log: s.log.With("source", "registry")})
	if s.catalog.CompareAndSwap(cur, next) {
		return next
	}
	return s.catalog.Load()
}
