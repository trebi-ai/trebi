package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/registry"
)

//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeConnectors(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connectors", s.handleConnectors)
	mux.HandleFunc("POST /v1/connectors", s.handleConnectorsCreate)
	mux.HandleFunc("GET /v1/connectors/catalog", s.handleConnectorsCatalog)
	mux.HandleFunc("GET /v1/connectors/catalog/entry", s.handleConnectorCatalogEntry)
	mux.HandleFunc("POST /v1/connectors/install", s.handleConnectorInstall)
	mux.HandleFunc("POST /v1/connectors/check-updates", s.handleConnectorCheckUpdates)
	mux.HandleFunc("POST /v1/connectors/gc", s.handleConnectorGC)
	mux.HandleFunc("GET /v1/connectors/{name}", s.handleConnectorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}", s.handleConnectorPut)
	mux.HandleFunc("PATCH /v1/connectors/{name}", s.handleConnectorPatch)
	mux.HandleFunc("DELETE /v1/connectors/{name}", s.handleConnectorDelete)
	mux.HandleFunc("POST /v1/connectors/{name}/connect", s.handleConnectorStart)
	mux.HandleFunc("POST /v1/connectors/{name}/disconnect", s.handleConnectorStop)
	mux.HandleFunc("POST /v1/connectors/{name}/restart", s.handleConnectorRestart)
	mux.HandleFunc("POST /v1/connectors/{name}/test", s.handleConnectorTest)
	mux.HandleFunc("GET /v1/connectors/{name}/logs", s.handleConnectorLogs)
	mux.HandleFunc("GET /v1/connectors/{name}/versions", s.handleConnectorVersions)
	mux.HandleFunc("POST /v1/connectors/{name}/check-updates", s.handleConnectorCheckUpdates)
	mux.HandleFunc("POST /v1/connectors/{name}/update", s.handleConnectorUpdate)
	mux.HandleFunc("GET /v1/connectors/{name}/capabilities", s.handleConnectorCapabilities)
	mux.HandleFunc("GET /v1/connectors/{name}/cursor", s.handleConnectorCursorGet)
	mux.HandleFunc("PUT /v1/connectors/{name}/cursor", s.handleConnectorCursorPut)
	s.routeConnectorSessions(mux)
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
	list := s.connectors.List(s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
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
	var d connectors.Entry
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
		writeJSON(w, http.StatusCreated, map[string]any{"name": d.Name})
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
	row, err := s.connectors.GetFor(r.PathValue("name"), s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
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
	var d connectors.Entry
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
	var p connectors.EntryPatch
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
	return s.connectors.HasEvents(name)
}

// eventsInstance checks the events block and picks the instance.
func (s *Server) eventsInstance(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	if err := s.requireEventsBlock(name); err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return "", false
	}
	inst, err := s.connectorInstance(r, name)
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return "", false
	}
	return inst, true
}

func (s *Server) handleConnectorStart(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.eventsInstance(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	if err := s.connectors.Start(r.Context(), inst); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "listening"})
}

func (s *Server) handleConnectorStop(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.eventsInstance(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	if err := s.connectors.Stop(inst); err != nil {
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
	inst, ok := s.eventsInstance(w, r, name)
	if !ok {
		return
	}
	if err := s.connectors.Restart(r.Context(), inst); err != nil {
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
	inst, ok := s.eventsInstance(w, r, name)
	if !ok {
		return
	}
	path := connectors.LogPath(inst)
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
	inst, ok := s.eventsInstance(w, r, name)
	if !ok {
		return
	}
	cur, err := s.connectors.GetCursor(r.Context(), inst)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var updated any
	if cur.UpdatedAt != "" {
		updated = cur.UpdatedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "instance": inst, "last_event_id": cur.LastEventID, "updated_at": updated})
}

func (s *Server) handleConnectorCursorPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	inst, ok := s.eventsInstance(w, r, name)
	if !ok {
		return
	}
	var body struct {
		LastEventID string `json:"last_event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body sets last_event_id to ""
	if err := s.connectors.PutCursor(r.Context(), inst, body.LastEventID); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "instance": inst, "last_event_id": body.LastEventID})
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
	case errors.Is(err, connectors.ErrReserved),
		errors.Is(err, connectors.ErrDisabled):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrNoCatalog):
		writeErr(w, http.StatusConflict, "precondition_failed", err.Error())
	default:
		return writeProtocolErr(w, err)
	}
	return true
}

// writeProtocolErr maps an adapter error to a status code.
func writeProtocolErr(w http.ResponseWriter, err error) bool {
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case protocol.CodeNotFound:
		writeErr(w, http.StatusNotFound, "not_found", pe.Message)
	case protocol.CodeInvalid, protocol.CodeUnsupported:
		writeErr(w, http.StatusBadRequest, "validation", pe.Message)
	case protocol.CodeTransient, protocol.CodeAuthRequired, protocol.CodeRateLimited:
		writeErr(w, http.StatusConflict, "conflict", pe.Message)
	default:
		writeErr(w, http.StatusBadGateway, "upstream", pe.Message)
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
	if len(res.Stale) > 0 {
		resp["stale_registries"] = res.Stale
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

// handleConnectorInstall installs one registry entry as a connector. A Trebi
// catalog entry installs its signed snapshot. Other registry data is
// untrusted: the entry becomes an inline manifest that passes the same
// validation as any other entry. Secret inputs go to the secret store.
func (s *Server) handleConnectorInstall(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Registry string            `json:"registry"`
		Name     string            `json:"name"`
		Version  string            `json:"version"`
		Track    string            `json:"track"`
		As       string            `json:"as"`
		Package  *int              `json:"package"`
		Remote   *int              `json:"remote"`
		Inputs   map[string]string `json:"inputs"`
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
	entry, err := s.catalogRegistry().Get(r.Context(), body.Registry, body.Name, body.Version)
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	if entry.Catalog != nil {
		if body.As != "" || body.Package != nil || body.Remote != nil {
			writeErr(w, http.StatusBadRequest, "validation", "as, package, and remote apply to MCP registry entries only")
			return
		}
		req := connectors.InstallRequest{Name: body.Name, Version: entry.Server.Version, Track: body.Track, Inputs: body.Inputs}
		if _, err := s.connectors.Install(r.Context(), req); err != nil {
			s.writeInstallErr(w, err)
			return
		}
		s.writeConnectorRow(w, http.StatusCreated, body.Name)
		return
	}
	if body.Track != "" {
		writeErr(w, http.StatusBadRequest, "validation", "track applies to Trebi catalog entries only")
		return
	}
	m, inputs, err := entry.Manifest(registry.InstallOpts{
		Name:    body.As,
		Package: indexOr(body.Package),
		Remote:  indexOr(body.Remote),
		Env:     body.Inputs,
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	e := connectors.Entry{Name: m.Name, From: connectors.FromInline, Manifest: &m, Inputs: inputs}
	if err := s.connectors.Create(r.Context(), e); err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	s.writeConnectorRow(w, http.StatusCreated, m.Name)
}

// writeConnectorRow writes the row of one connector after a write.
func (s *Server) writeConnectorRow(w http.ResponseWriter, status int, name string) {
	row, err := s.connectors.Get(name)
	if err != nil {
		row = map[string]any{"name": name}
	}
	writeJSON(w, status, row)
}

// writeInstallErr maps an install or update error. A catalog failure is a
// registry error; anything else is a connector error or upstream.
func (s *Server) writeInstallErr(w http.ResponseWriter, err error) {
	switch {
	case s.writeConnectorErr(w, err):
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrUnknownRegistry),
		errors.Is(err, registry.ErrInvalidEntry), errors.Is(err, registry.ErrBadSignature):
		s.writeRegistryErr(w, err)
	default:
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
	}
}

// handleConnectorVersions lists the catalog versions of one connector.
func (s *Server) handleConnectorVersions(w http.ResponseWriter, r *http.Request) {
	vers, err := s.connectors.Versions(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeInstallErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vers})
}

// handleConnectorCheckUpdates checks one connector, or all of them with no
// name, against the catalog now.
func (s *Server) handleConnectorCheckUpdates(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	res, err := s.connectors.CheckUpdates(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeInstallErr(w, err)
		return
	}
	if res == nil {
		res = []connectors.UpdateResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// handleConnectorUpdate moves the global entry, or one workspace override,
// to a catalog version. No version is the newest on the track.
func (s *Server) handleConnectorUpdate(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Version   string `json:"version"`
		Workspace string `json:"workspace"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	if err := s.connectors.Update(r.Context(), name, s.workspaceID(r.Context(), body.Workspace), body.Version); err != nil {
		s.writeInstallErr(w, err)
		return
	}
	s.writeConnectorRow(w, http.StatusOK, name)
}

// handleConnectorCapabilities inspects one connector instance.
func (s *Server) handleConnectorCapabilities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.connectors.Capabilities(r.Context(), r.PathValue("name"), s.workspaceID(r.Context(), q.Get("workspace")), q.Get("instance"))
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleConnectorGC removes installed versions that no entry uses.
func (s *Server) handleConnectorGC(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		DryRun bool `json:"dry_run"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	removed, err := s.connectors.GC(r.Context(), body.DryRun)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if removed == nil {
		removed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "dry_run": body.DryRun})
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
	if c := e.Catalog; c != nil {
		row["publisher"] = c.Publisher
		row["capabilities"] = c.Capabilities
		row["versions"] = c.Versions
		if c.IconURL != "" {
			row["icon_url"] = c.IconURL
		}
		return row
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

// catalogRegistry returns the registry client for the current config.
func (s *Server) catalogRegistry() *registry.Service {
	return s.catalog.Current()
}
