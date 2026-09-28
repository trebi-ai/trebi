package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// routeConnectorSessions serves the live side of a connector: login, rooms,
// and the workspace layer. Login steps never reach the bus or a log.
//
//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeConnectorSessions(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/connectors/{name}/login", s.handleConnectorLogin)
	mux.HandleFunc("GET /v1/connectors/{name}/login/{flow_id}", s.handleConnectorLoginStream)
	mux.HandleFunc("POST /v1/connectors/{name}/login/{flow_id}", s.handleConnectorLoginSubmit)
	mux.HandleFunc("DELETE /v1/connectors/{name}/login/{flow_id}", s.handleConnectorLoginCancel)
	mux.HandleFunc("POST /v1/connectors/{name}/logout", s.handleConnectorLogout)
	mux.HandleFunc("POST /v1/connectors/{name}/conformance", s.handleConnectorConformance)
	mux.HandleFunc("GET /v1/connectors/{name}/rooms", s.handleConnectorRooms)
	mux.HandleFunc("GET /v1/connectors/{name}/rooms/{room}/threads", s.handleConnectorThreads)
	mux.HandleFunc("POST /v1/connectors/{name}/rooms/{room}/bind", s.handleConnectorBind)
	mux.HandleFunc("GET /v1/workspaces/{id}/connectors/{name}", s.handleConnectorOverrideGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/connectors/{name}", s.handleConnectorOverridePut)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/connectors/{name}", s.handleConnectorOverrideDelete)
}

// pickInstance picks an instance: the given one, else the instance that
// serves the workspace, else the global instance.
func (s *Server) pickInstance(r *http.Request, name, instance, workspace string) (string, error) {
	if instance = strings.TrimSpace(instance); instance != "" {
		if n, _, _ := strings.Cut(instance, "@"); n != name {
			return "", fmt.Errorf("%w: instance %q is not an instance of %q", connectors.ErrInvalid, instance, name)
		}
		return instance, nil
	}
	if ws := s.workspaceID(r.Context(), workspace); ws != "" {
		inst := s.connectors.InstanceFor(name, ws)
		if inst == "" {
			return "", fmt.Errorf("%w: %q in %s", connectors.ErrDisabled, name, ws)
		}
		return inst, nil
	}
	return name, nil
}

// connectorInstance picks the instance from ?instance= and ?workspace=.
func (s *Server) connectorInstance(r *http.Request, name string) (string, error) {
	q := r.URL.Query()
	return s.pickInstance(r, name, q.Get("instance"), q.Get("workspace"))
}

// writeAnyConnectorErr writes a connector or adapter error, else a 400.
func (s *Server) writeAnyConnectorErr(w http.ResponseWriter, err error) {
	if !s.writeConnectorErr(w, err) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	}
}

// decodeBody decodes an optional JSON body with no unknown fields.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return false
	}
	return true
}

func (s *Server) handleConnectorLogin(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name := r.PathValue("name")
	var body struct {
		Instance  string `json:"instance"`
		Workspace string `json:"workspace"`
		Kind      string `json:"kind"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	inst, err := s.pickInstance(r, name, body.Instance, body.Workspace)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	f, err := s.connectors.Login(r.Context(), inst, body.Kind)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	cur := f.Current()
	writeJSON(w, http.StatusOK, map[string]any{"flow_id": f.ID, "instance": inst, "step": cur.Step, "done": cur.Done})
}

// loginFlow finds the flow of the route, or writes 404.
func (s *Server) loginFlow(w http.ResponseWriter, r *http.Request) (*connectors.Flow, bool) {
	f, ok := s.connectors.FindFlow(r.PathValue("name"), r.PathValue("flow_id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "login flow not found")
	}
	return f, ok
}

// handleConnectorLoginStream streams the flow as SSE: one step event per
// step, then one done event.
func (s *Server) handleConnectorLoginStream(w http.ResponseWriter, r *http.Request) {
	f, ok := s.loginFlow(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusOK, f.Current())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for u := range f.Subscribe(r.Context()) {
		event := "step"
		if u.Done != nil {
			event = "done"
		}
		b, err := json.Marshal(u)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
}

func (s *Server) handleConnectorLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	f, ok := s.loginFlow(w, r)
	if !ok {
		return
	}
	var body struct {
		Fields map[string]string `json:"fields"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.connectors.SubmitLogin(r.Context(), f, body.Fields); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f.Current())
}

func (s *Server) handleConnectorLoginCancel(w http.ResponseWriter, r *http.Request) {
	f, ok := s.loginFlow(w, r)
	if !ok {
		return
	}
	if err := s.connectors.CancelLogin(r.Context(), f); err != nil {
		s.log.Warn("connector login cancel", "connector", f.Instance, "err", err)
	}
	writeJSON(w, http.StatusOK, f.Current())
}

func (s *Server) handleConnectorLogout(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name := r.PathValue("name")
	var body struct {
		Instance  string `json:"instance"`
		Workspace string `json:"workspace"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	inst, err := s.pickInstance(r, name, body.Instance, body.Workspace)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	if err := s.connectors.Logout(r.Context(), inst); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": inst, "logged_out": true})
}

// handleConnectorConformance runs the Part H checks against a live
// instance. It sends a message only to the room in the body. A failed check
// is a result row, not an error status.
func (s *Server) handleConnectorConformance(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name := r.PathValue("name")
	var body struct {
		Instance  string `json:"instance"`
		Workspace string `json:"workspace"`
		Room      string `json:"room"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	inst, err := s.pickInstance(r, name, body.Instance, body.Workspace)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	res, err := s.connectors.Conformance(r.Context(), inst, body.Room)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	ok := true
	for _, c := range res {
		ok = ok && c.Status != protocol.CheckFail
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "instance": inst, "ok": ok, "results": res})
}

// handleConnectorRooms lists rooms. Email serves its addresses; a protocol
// instance answers rooms/list.
func (s *Server) handleConnectorRooms(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	if name == email.Instance {
		rooms, err := s.email.Rooms(r.Context(), q.Get("q"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"instance": email.Instance, "rooms": nonNil(rooms)})
		return
	}
	inst, err := s.connectorInstance(r, name)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	var page protocol.RoomPage
	params := protocol.RoomsListParams{Query: q.Get("q"), Kind: q.Get("kind"), Page: q.Get("cursor"), Limit: parseLimit(r)}
	if err := s.connectors.Call(r.Context(), inst, protocol.MethodRoomsList, params, &page); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	resp := map[string]any{"instance": inst, "rooms": nonNil(page.Rooms)}
	if page.Next != "" {
		resp["next_cursor"] = page.Next
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConnectorThreads lists the threads of a room. An adapter without
// threads/list, and email, read the threads the daemon saw.
func (s *Server) handleConnectorThreads(w http.ResponseWriter, r *http.Request) {
	name, room := r.PathValue("name"), r.PathValue("room")
	inst := email.Instance
	if name != email.Instance {
		var err error
		if inst, err = s.connectorInstance(r, name); err != nil {
			s.writeAnyConnectorErr(w, err)
			return
		}
		var page protocol.ThreadPage
		params := protocol.ThreadsListParams{Room: room, Page: r.URL.Query().Get("cursor"), Limit: parseLimit(r)}
		err = s.connectors.Call(r.Context(), inst, protocol.MethodThreadsList, params, &page)
		if err == nil {
			resp := map[string]any{"instance": inst, "threads": nonNil(page.Threads)}
			if page.Next != "" {
				resp["next_cursor"] = page.Next
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if protocol.CodeOf(err) != protocol.CodeUnsupported {
			s.writeAnyConnectorErr(w, err)
			return
		}
	}
	threads, err := s.connectors.KnownThreads(r.Context(), inst, room, parseLimit(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": inst, "threads": threads})
}

// handleConnectorBind binds a room to the assistant or an agent of a
// workspace. The operator is "assistant" or "agents/<name>".
func (s *Server) handleConnectorBind(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	name, room := r.PathValue("name"), r.PathValue("room")
	var body struct {
		Workspace string `json:"workspace"`
		Instance  string `json:"instance"`
		Thread    string `json:"thread"`
		Operator  string `json:"operator"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ws := s.workspaceID(r.Context(), body.Workspace)
	if ws == "" {
		writeErr(w, http.StatusBadRequest, "validation", "workspace required")
		return
	}
	operator := strings.TrimSpace(body.Operator)
	switch {
	case operator == "" || operator == "assistant":
		operator = ""
	case !strings.HasPrefix(operator, "agents/"):
		writeErr(w, http.StatusBadRequest, "validation", `operator must be "assistant" or "agents/<name>"`)
		return
	}
	inst := email.Instance
	if name != email.Instance {
		var err error
		if inst, err = s.pickInstance(r, name, body.Instance, ws); err != nil {
			s.writeAnyConnectorErr(w, err)
			return
		}
		if !s.connectors.IsChannel(name) {
			writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("connector %q is not a channel", name))
			return
		}
	}
	p, err := s.conv.BindRoom(r.Context(), ws, operator, conversations.Address{
		Connector: name, Instance: inst, Room: room, Thread: body.Thread,
	})
	if s.convErr(w, err, "workspace not found") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleConnectorOverrideGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	ov, found, err := s.connectors.Override(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "workspace has no layer for this connector")
		return
	}
	ov.Inputs = connectors.MaskInputs(ov.Inputs)
	writeJSON(w, http.StatusOK, map[string]any{"workspace": ws.ID, "name": r.PathValue("name"), "override": ov})
}

func (s *Server) handleConnectorOverridePut(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	var ov workspaces.ConnectorOverride
	if !decodeBody(w, r, &ov) {
		return
	}
	name := r.PathValue("name")
	if err := s.connectors.SetOverride(r.Context(), ws.ID, name, &ov); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	row, err := s.connectors.GetFor(name, ws.ID)
	if err != nil {
		row = map[string]any{"name": name}
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleConnectorOverrideDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.connectors.SetOverride(r.Context(), ws.ID, name, nil); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": ws.ID, "deleted": name})
}

// nonNil turns a nil slice into an empty one, so JSON shows [].
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
