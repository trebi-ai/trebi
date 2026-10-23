package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	mux.HandleFunc("POST /v1/connectors/sources/check", s.handleConnectorSourceCheck)
	mux.HandleFunc("POST /v1/connectors/check-updates", s.handleConnectorCheckUpdates)
	mux.HandleFunc("POST /v1/connectors/gc", s.handleConnectorGC)
	mux.HandleFunc("GET /v1/connectors/packages", s.handleConnectorPackages)
	mux.HandleFunc("POST /v1/connectors/packages", s.handleConnectorPackageInstall)
	mux.HandleFunc("PUT /v1/connectors/packages/keep", s.handleConnectorPackagesKeep)
	mux.HandleFunc("GET /v1/connectors/{ref}", s.handleConnectorGet)
	mux.HandleFunc("PATCH /v1/connectors/{ref}", s.handleConnectorPatch)
	mux.HandleFunc("DELETE /v1/connectors/{ref}", s.handleConnectorDelete)
	mux.HandleFunc("POST /v1/connectors/{ref}/connect", s.handleConnectorStart)
	mux.HandleFunc("POST /v1/connectors/{ref}/disconnect", s.handleConnectorStop)
	mux.HandleFunc("POST /v1/connectors/{ref}/restart", s.handleConnectorRestart)
	mux.HandleFunc("POST /v1/connectors/{ref}/test", s.handleConnectorTest)
	mux.HandleFunc("GET /v1/connectors/{ref}/logs", s.handleConnectorLogs)
	mux.HandleFunc("GET /v1/connectors/{ref}/versions", s.handleConnectorVersions)
	mux.HandleFunc("POST /v1/connectors/{ref}/check-updates", s.handleConnectorCheckUpdates)
	mux.HandleFunc("POST /v1/connectors/{ref}/update", s.handleConnectorUpdate)
	mux.HandleFunc("GET /v1/connectors/{ref}/capabilities", s.handleConnectorCapabilities)
	mux.HandleFunc("GET /v1/connectors/{ref}/cursor", s.handleConnectorCursorGet)
	mux.HandleFunc("PUT /v1/connectors/{ref}/cursor", s.handleConnectorCursorPut)
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
		return fmt.Sprint(list[i]["slug"]) < fmt.Sprint(list[j]["slug"])
	})
	// Slug-sorted collection: limit only, no ULID cursor.
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
	var body struct {
		connectors.Connection
		URL   string `json:"url"`   // an online MCP server; Trebi writes the manifest
		Login bool   `json:"login"` // start the login after the add
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.URL != "" {
		s.createOnline(w, r, body.Connection, body.URL, body.Login)
		return
	}
	c := body.Connection
	if c.ID != "" {
		writeErr(w, http.StatusBadRequest, "validation", "id is set by the daemon")
		return
	}
	if c.Type == "" && c.Manifest != nil {
		c.Type = c.Manifest.Name
	}
	if c.Type == "" {
		writeErr(w, http.StatusBadRequest, "validation", "type required")
		return
	}
	var err error
	if c.Scope, err = s.scopeIDs(r.Context(), c.Scope); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	created, err := s.connectors.Create(r.Context(), c)
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	s.writeConnectorRow(w, http.StatusCreated, created.ID)
}

// createOnline adds an online MCP server by its URL. With login, it also
// starts the login and returns the flow under "login".
func (s *Server) createOnline(w http.ResponseWriter, r *http.Request, c connectors.Connection, mcpURL string, login bool) {
	if c.ID != "" || c.Type != "" || c.Manifest != nil {
		writeErr(w, http.StatusBadRequest, "validation", "url excludes id, type, and manifest")
		return
	}
	var err error
	if c.Scope, err = s.scopeIDs(r.Context(), c.Scope); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	created, err := s.connectors.CreateOnline(r.Context(), c, mcpURL)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	row, err := s.connectors.Get(created.ID)
	if err != nil {
		row = map[string]any{"id": created.ID}
	}
	if login && created.OAuth != nil {
		if f, err := s.connectors.Login(r.Context(), created.ID, connectors.LoginOAuth); err != nil {
			row["login"] = map[string]any{"error": err.Error()}
		} else {
			cur := f.Current()
			row["login"] = map[string]any{"flow_id": f.ID, "step": cur.Step, "done": cur.Done}
		}
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleConnectorGet(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("ref") {
	case "email":
		if row := s.email.ConnectorRow(); row != nil {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "email is not configured")
		return
	case connectors.NativeWebhook:
		if row := s.hooks.ConnectorRow(); row != nil {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "webhook is not available")
		return
	}
	row, err := s.connectors.GetFor(r.PathValue("ref"), s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorPatch(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var p connectors.ConnectionPatch
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if len(p.Scope) > 0 {
		sc, err := p.ScopeValue()
		if err == nil {
			sc, err = s.scopeIDs(r.Context(), sc)
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "scope: "+err.Error())
			return
		}
		p.Scope, _ = json.Marshal(sc) //nolint:errcheck // a string list always encodes
	}
	id := s.connectors.IDOf(r.PathValue("ref"))
	if id == "" {
		id = r.PathValue("ref")
	}
	changed, err := s.connectors.Patch(r.Context(), id, p)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	row, err := s.connectors.Get(id)
	if err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(changed) > 0 {
		row["changed_files"] = changed
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	if err := s.connectors.Delete(r.Context(), r.PathValue("ref")); err != nil {
		if s.writeConnectorErr(w, err) {
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("ref")})
}

// requireEventsBlock refuses lifecycle and stream routes on a connector with
// no events block. Native connectors carry events in their own service.
func (s *Server) requireEventsBlock(ref string) error {
	if ref == connectors.NativeEmail || ref == connectors.NativeWebhook {
		return nil
	}
	return s.connectors.HasEvents(ref)
}

// eventsInstance checks the events block and returns the connection id.
func (s *Server) eventsInstance(w http.ResponseWriter, r *http.Request) (string, bool) {
	if err := s.requireEventsBlock(r.PathValue("ref")); err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return "", false
	}
	inst, err := s.connectionID(r)
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return "", false
	}
	return inst, true
}

func (s *Server) handleConnectorStart(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.eventsInstance(w, r)
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
	inst, ok := s.eventsInstance(w, r)
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
	if r.PathValue("ref") == connectors.NativeEmail {
		s.handleEmailPoll(w, r)
		return
	}
	inst, ok := s.eventsInstance(w, r)
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
	name := r.PathValue("ref")
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

// connectorLogTail is the count of old log lines that the log route sends first.
const connectorLogTail = 1000

func (s *Server) handleConnectorLogs(w http.ResponseWriter, r *http.Request) {
	// The native channel has no process, so it has no process log. Its
	// activity is the email log (plan K).
	if err := s.connectors.GuardReserved(r.PathValue("ref")); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	inst, ok := s.eventsInstance(w, r)
	if !ok {
		return
	}
	lr, err := s.connectors.OpenLog(inst)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	defer lr.Close()
	tail, err := lr.Tail(connectorLogTail)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		log := strings.Join(tail, "\n")
		if len(tail) > 0 {
			log += "\n"
		}
		writeJSON(w, http.StatusOK, map[string]any{"log": log})
		return
	}
	// Headers must go out before the first log line, or an empty file + follow
	// holds the connection with no response and proxies time out as 502.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	send := func(lines []string) {
		for _, line := range lines {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		flusher.Flush()
	}
	send(tail)

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
			if lines, _ := lr.Next(); len(lines) > 0 { // a read fault tries again on the next tick
				send(lines)
			}
		}
	}
}

func (s *Server) handleConnectorCursorGet(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.eventsInstance(w, r)
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
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "slug": s.connectors.SlugOf(inst), "last_event_id": cur.LastEventID, "updated_at": updated})
}

func (s *Server) handleConnectorCursorPut(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.eventsInstance(w, r)
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
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "slug": s.connectors.SlugOf(inst), "last_event_id": body.LastEventID})
}

// scopeIDs maps the workspace names of a scope to ids. global stays. An
// unknown workspace is an error.
func (s *Server) scopeIDs(ctx context.Context, sc connectors.Scope) (connectors.Scope, error) {
	if len(sc) == 0 {
		return nil, nil
	}
	out := make(connectors.Scope, 0, len(sc))
	for _, v := range sc {
		v = strings.TrimSpace(v)
		if v == connectors.ScopeGlobal {
			out = append(out, v)
			continue
		}
		ws, err := s.ws.Resolve(ctx, v)
		if err != nil {
			return nil, fmt.Errorf("workspace %q not found", v)
		}
		out = append(out, ws.ID)
	}
	return out, nil
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
	case errors.Is(err, connectors.ErrNotFound), errors.Is(err, connectors.ErrSubscriptionNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, connectors.ErrNoSubscriptions):
		writeErr(w, http.StatusBadRequest, "unsupported", err.Error())
	case errors.Is(err, connectors.ErrExists),
		errors.Is(err, connectors.ErrLoadFailed),
		errors.Is(err, connectors.ErrSlugRewrite):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrNoEvents):
		writeErr(w, http.StatusConflict, "validation", err.Error())
	case errors.Is(err, connectors.ErrInherited):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, connectors.ErrReserved),
		errors.Is(err, connectors.ErrDisabled),
		errors.Is(err, connectors.ErrNotInScope):
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

// handleConnectorInstall installs one registry entry as a new connection. A
// Trebi catalog entry installs its signed snapshot. Other registry data is
// untrusted: the entry becomes an inline manifest that passes the same
// validation as any other connection. A literal secret input goes to the
// folder of the connection.
func (s *Server) handleConnectorInstall(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Registry string                    `json:"registry"`
		Name     string                    `json:"name"`
		Version  string                    `json:"version"`
		Track    string                    `json:"track"`
		Slug     string                    `json:"slug"`
		Title    string                    `json:"title"`
		Package  *int                      `json:"package"`
		Remote   *int                      `json:"remote"`
		Inputs   map[string]string         `json:"inputs"`
		Source   *connectors.SourceInstall `json:"source"`
		Force    bool                      `json:"force"`
		// Scope names the workspaces that see the connection; Workspace is
		// the workspace of the caller, the default scope of an account.
		Scope     connectors.Scope `json:"scope"`
		Workspace string           `json:"workspace"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	scope, err := s.scopeIDs(r.Context(), body.Scope)
	if err == nil && body.Workspace != "" {
		var ids connectors.Scope
		ids, err = s.scopeIDs(r.Context(), connectors.Scope{body.Workspace})
		if err == nil {
			body.Workspace = ids[0]
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Source != nil || body.Force {
		if body.Name != "" || body.Registry != "" || body.Track != "" || body.Package != nil || body.Remote != nil {
			writeErr(w, http.StatusBadRequest, "validation", "source does not go with name, registry, track, package, or remote")
			return
		}
		req := connectors.InstallRequest{Source: body.Source, Force: body.Force, Version: body.Version,
			Inputs: body.Inputs, Slug: body.Slug, Title: body.Title, Scope: scope, Workspace: body.Workspace}
		c, err := s.connectors.Install(r.Context(), req)
		if err != nil {
			s.writeInstallErr(w, err)
			return
		}
		s.writeConnectorRow(w, http.StatusCreated, c.ID)
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
		if body.Package != nil || body.Remote != nil {
			writeErr(w, http.StatusBadRequest, "validation", "package and remote apply to MCP registry entries only")
			return
		}
		req := connectors.InstallRequest{
			Name: body.Name, Version: entry.Server.Version, Track: body.Track, Inputs: body.Inputs,
			Slug: body.Slug, Title: body.Title, Scope: scope, Workspace: body.Workspace,
		}
		c, err := s.connectors.Install(r.Context(), req)
		if err != nil {
			s.writeInstallErr(w, err)
			return
		}
		s.writeConnectorRow(w, http.StatusCreated, c.ID)
		return
	}
	if body.Track != "" {
		writeErr(w, http.StatusBadRequest, "validation", "track applies to Trebi catalog entries only")
		return
	}
	m, inputs, err := entry.Manifest(registry.InstallOpts{
		Package: indexOr(body.Package),
		Remote:  indexOr(body.Remote),
		Env:     body.Inputs,
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	c := connectors.Connection{Type: m.Name, Slug: body.Slug, Title: body.Title, From: connectors.FromInline, Manifest: &m, Inputs: inputs, Scope: scope}
	created, err := s.connectors.Create(r.Context(), c.WithDefaultScope(body.Workspace))
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	s.writeConnectorRow(w, http.StatusCreated, created.ID)
}

// handleConnectorSourceCheck fetches and checks the folder of a GitHub link.
// It installs nothing.
func (s *Server) handleConnectorSourceCheck(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.URL) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "url required")
		return
	}
	check, err := s.connectors.CheckSource(r.Context(), body.URL)
	if err != nil {
		s.writeInstallErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, check)
}

// writeSourceErr maps a link error. It reports false for any other error.
func writeSourceErr(w http.ResponseWriter, err error) bool {
	var ce *connectors.InstallCheckError
	var se *connectors.SourceError
	switch {
	case errors.As(err, &ce):
		writeErrDetails(w, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]any{"reason": ce.Reason, "results": ce.Results})
	case errors.Is(err, connectors.ErrSourceNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, connectors.ErrSourceTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, connectors.ErrSourceRateLimited):
		retry := 0
		if errors.As(err, &se) {
			retry = int(se.RetryAfter.Seconds())
		}
		writeErrDetails(w, http.StatusTooManyRequests, "rate_limited", err.Error(), map[string]any{"retry_after": retry})
	case errors.Is(err, connectors.ErrSourceUnavailable):
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
	default:
		return false
	}
	return true
}

// writeConnectorRow writes the row of one connection after a write.
func (s *Server) writeConnectorRow(w http.ResponseWriter, status int, id string) {
	row, err := s.connectors.Get(id)
	if err != nil {
		row = map[string]any{"id": id}
	}
	writeJSON(w, status, row)
}

// writeInstallErr maps an install or update error. A catalog failure is a
// registry error; anything else is a connector error or upstream.
func (s *Server) writeInstallErr(w http.ResponseWriter, err error) {
	switch {
	case writeSourceErr(w, err):
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
	vers, err := s.connectors.Versions(r.Context(), r.PathValue("ref"))
	if err != nil {
		s.writeInstallErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vers})
}

// handleConnectorCheckUpdates checks one connection, or all of them with no
// ref, against the catalog now.
func (s *Server) handleConnectorCheckUpdates(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	res, err := s.connectors.CheckUpdates(r.Context(), r.PathValue("ref"))
	if err != nil {
		s.writeInstallErr(w, err)
		return
	}
	if res == nil {
		res = []connectors.UpdateResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// handleConnectorUpdate moves a connection to a catalog version. No version
// is the newest on the track.
func (s *Server) handleConnectorUpdate(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ref := r.PathValue("ref")
	if err := s.connectors.Update(r.Context(), ref, body.Version); err != nil {
		s.writeInstallErr(w, err)
		return
	}
	s.writeConnectorRow(w, http.StatusOK, s.connectors.IDOf(ref))
}

// handleConnectorCapabilities inspects one connection. ?workspace= applies
// the workspace layer.
func (s *Server) handleConnectorCapabilities(w http.ResponseWriter, r *http.Request) {
	c, err := s.connectors.Capabilities(r.Context(), r.PathValue("ref"), s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleConnectorGC removes installed packages that no connection uses.
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

// handleConnectorPackages lists the installed packages.
func (s *Server) handleConnectorPackages(w http.ResponseWriter, r *http.Request) {
	pkgs, err := s.connectors.Packages(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": pkgs})
}

// handleConnectorPackageInstall installs one package with no connection.
func (s *Server) handleConnectorPackageInstall(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Source *connectors.PackageSource `json:"source"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Source == nil {
		writeErr(w, http.StatusBadRequest, "validation", "source required")
		return
	}
	pkg, err := s.connectors.InstallPackage(r.Context(), *body.Source)
	if err != nil {
		s.writeInstallErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pkg)
}

// handleConnectorPackagesKeep replaces the packages that one holder keeps.
func (s *Server) handleConnectorPackagesKeep(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Holder   string   `json:"holder"`
		Packages []string `json:"packages"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	kept, err := s.connectors.KeepPackages(r.Context(), body.Holder, body.Packages)
	if err != nil {
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holder": body.Holder, "packages": kept})
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
