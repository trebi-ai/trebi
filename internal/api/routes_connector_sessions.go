package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/conformance"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// maxLoginWait bounds the wait of a login status read, in seconds.
const maxLoginWait = 60

// routeConnectorSessions serves the live side of a connector: login, rooms,
// and the workspace layer. Login steps never reach the bus or a log.
//
//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeConnectorSessions(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connectors/{ref}/login/methods", s.handleConnectorLoginMethods)
	mux.HandleFunc("POST /v1/connectors/{ref}/login", s.handleConnectorLogin)
	mux.HandleFunc("GET /v1/connectors/{ref}/login/{flow_id}", s.handleConnectorLoginStream)
	mux.HandleFunc("POST /v1/connectors/{ref}/login/{flow_id}", s.handleConnectorLoginSubmit)
	mux.HandleFunc("DELETE /v1/connectors/{ref}/login/{flow_id}", s.handleConnectorLoginCancel)
	mux.HandleFunc("POST /v1/connectors/{ref}/logout", s.handleConnectorLogout)
	mux.HandleFunc("GET /v1/connectors/{ref}/oauth", s.handleConnectorOAuthGet)
	mux.HandleFunc("PATCH /v1/connectors/{ref}/oauth", s.handleConnectorOAuthPatch)
	mux.HandleFunc("POST /v1/connectors/{ref}/conformance", s.handleConnectorConformance)
	mux.HandleFunc("GET /v1/connectors/{ref}/rooms", s.handleConnectorRooms)
	mux.HandleFunc("GET /v1/connectors/{ref}/rooms/{room}/threads", s.handleConnectorThreads)
	mux.HandleFunc("POST /v1/connectors/{ref}/rooms/{room}/bind", s.handleConnectorBind)
	mux.HandleFunc("GET /v1/connectors/{ref}/senders", s.handleConnectorSenders)
	mux.HandleFunc("GET /v1/connectors/{ref}/dependents", s.handleConnectorDependents)
	mux.HandleFunc("GET /v1/connectors/{ref}/subscriptions", s.handleConnectorSubscriptions)
	mux.HandleFunc("POST /v1/connectors/{ref}/subscriptions", s.handleConnectorSubscribe)
	mux.HandleFunc("PATCH /v1/connectors/{ref}/subscriptions/{id}", s.handleConnectorSubscriptionPatch)
	mux.HandleFunc("DELETE /v1/connectors/{ref}/subscriptions/{id}", s.handleConnectorUnsubscribe)
	mux.HandleFunc("POST /v1/connectors/{ref}/subscriptions/{id}/submit", s.handleConnectorSubscriptionSubmit)
	mux.HandleFunc("POST /v1/connectors/{ref}/subscriptions/sync", s.handleConnectorSubscriptionsSync)
	mux.HandleFunc("POST /v1/connectors/{ref}/subscriptions/options", s.handleConnectorSubscriptionOptions)
	mux.HandleFunc("GET /v1/workspaces/{id}/connectors/{ref}", s.handleConnectorOverrideGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/connectors/{ref}", s.handleConnectorOverridePut)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/connectors/{ref}", s.handleConnectorOverrideDelete)
}

// connectionID returns the id of the {ref} of a route. A native name is its
// own id.
func (s *Server) connectionID(r *http.Request) (string, error) {
	ref := r.PathValue("ref")
	if id := s.connectors.IDOf(ref); id != "" {
		return id, nil
	}
	_, err := s.connectors.Lookup(ref)
	return "", err
}

// instanceOf returns the instance of the {ref} of a route. Native email is
// its own instance.
func (s *Server) instanceOf(r *http.Request) (string, error) {
	if r.PathValue("ref") == email.Instance {
		return email.Instance, nil
	}
	return s.connectionID(r)
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

func (s *Server) handleConnectorLoginMethods(w http.ResponseWriter, r *http.Request) {
	inst, err := s.connectionID(r)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	methods, err := s.connectors.LoginMethods(r.Context(), inst)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "methods": methods})
}

// keyFormOnly reports a caller that must not handle a pasted key: the
// assistant, a remote client, or a run. A person in the UI, in the CLI, or
// on a paired phone passes.
func keyFormOnly(r *http.Request) bool {
	a, ok := tools.ActorFrom(r.Context())
	return ok && a.Kind != tools.ActorMobile
}

// loginForm is the UI route where a person pastes the key of a method.
func loginForm(id, method string) string {
	return "/connectors/" + url.PathEscape(id) + "?tab=login&method=" + url.QueryEscape(method)
}

// handleConnectorLogin starts a flow. A key method does not start for the
// assistant: the answer names the form where the person pastes the key.
func (s *Server) handleConnectorLogin(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Method string `json:"method"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	inst, err := s.connectionID(r)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	m, err := s.connectors.PickLoginMethod(r.Context(), inst, body.Method)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	if m.Type == protocol.LoginKey && keyFormOnly(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": inst, "method": m, "form": loginForm(inst, m.ID)})
		return
	}
	f, err := s.connectors.Login(r.Context(), inst, m.ID)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	cur := f.Current()
	writeJSON(w, http.StatusOK, map[string]any{"flow_id": f.ID, "id": inst, "method": m, "step": cur.Step, "done": cur.Done})
}

// loginFlow finds the flow of the route, or writes 404.
func (s *Server) loginFlow(w http.ResponseWriter, r *http.Request) (*connectors.Flow, bool) {
	f, ok := s.connectors.FindFlow(r.PathValue("ref"), r.PathValue("flow_id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "login flow not found")
	}
	return f, ok
}

// handleConnectorLoginStream streams the flow as SSE: one step event per
// step, then one done event. With ?wait=<seconds>, it waits up to that
// time for the end of the login and returns the frame as JSON.
func (s *Server) handleConnectorLoginStream(w http.ResponseWriter, r *http.Request) {
	f, ok := s.loginFlow(w, r)
	if !ok {
		return
	}
	if q := r.URL.Query().Get("wait"); q != "" {
		sec, err := strconv.Atoi(q)
		if err != nil || sec < 0 {
			writeErr(w, http.StatusBadRequest, "validation", "wait must be a number of seconds")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(min(sec, maxLoginWait))*time.Second)
		defer cancel()
		last := f.Current()
		for u := range f.Subscribe(ctx) {
			last = u
		}
		writeJSON(w, http.StatusOK, last)
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
	if f.Method().Type == protocol.LoginKey && keyFormOnly(r) {
		writeErr(w, http.StatusForbidden, "use_login_form", "Ask the person to paste the key in the login form: "+loginForm(f.Instance, f.Method().ID))
		return
	}
	var body struct {
		Fields map[string]protocol.Value `json:"fields"`
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
	inst, err := s.connectionID(r)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	if err := s.connectors.Logout(r.Context(), inst); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "logged_out": true})
}

func (s *Server) handleConnectorOAuthGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.connectors.OAuthStatus(r.PathValue("ref"))
	if err != nil {
		s.writeOAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleConnectorOAuthPatch(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var p connectors.OAuthPatch
	if !decodeBody(w, r, &p) {
		return
	}
	if err := s.connectors.SetOAuth(r.Context(), r.PathValue("ref"), p); err != nil {
		s.writeOAuthErr(w, err)
		return
	}
	v, err := s.connectors.OAuthStatus(r.PathValue("ref"))
	if err != nil {
		s.writeOAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) writeOAuthErr(w http.ResponseWriter, err error) {
	if errors.Is(err, connectors.ErrNoOAuth) {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	s.writeAnyConnectorErr(w, err)
}

// handleConnectorConformance runs the conformance suites against a live
// connection. It sends a message only to the room in the body. A failed
// check is a result row, not an error status.
func (s *Server) handleConnectorConformance(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Room   string   `json:"room"`
		Suites []string `json:"suites"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	inst, err := s.connectionID(r)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	res, err := s.connectors.Conformance(r.Context(), inst, body.Room, body.Suites)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	ok := true
	for _, c := range res {
		ok = ok && c.Status != conformance.StatusFail
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "slug": s.connectors.SlugOf(inst), "ok": ok, "results": res})
}

// handleConnectorRooms lists rooms. Email serves its addresses; a protocol
// instance answers rooms/list.
func (s *Server) handleConnectorRooms(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.PathValue("ref") == email.Instance {
		rooms, err := s.email.Rooms(r.Context(), q.Get("q"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": email.Instance, "rooms": nonNil(rooms)})
		return
	}
	inst, err := s.connectionID(r)
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
	resp := map[string]any{"id": inst, "rooms": nonNil(page.Rooms)}
	if page.Next != "" {
		resp["next_cursor"] = page.Next
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConnectorThreads lists the threads of a room. An adapter without
// threads/list, and email, read the threads the daemon saw.
func (s *Server) handleConnectorThreads(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	inst := email.Instance
	if r.PathValue("ref") != email.Instance {
		var err error
		if inst, err = s.connectionID(r); err != nil {
			s.writeAnyConnectorErr(w, err)
			return
		}
		if s.rejectNoThreads(w, inst) {
			return
		}
		var page protocol.ThreadPage
		params := protocol.ThreadsListParams{Room: room, Page: r.URL.Query().Get("cursor"), Limit: parseLimit(r)}
		err = s.connectors.Call(r.Context(), inst, protocol.MethodThreadsList, params, &page)
		if err == nil {
			resp := map[string]any{"id": inst, "threads": nonNil(page.Threads)}
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
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "threads": threads})
}

// rejectNoThreads writes 400 unsupported when the instance does not declare
// the threads feature (live session first, then the manifest).
func (s *Server) rejectNoThreads(w http.ResponseWriter, inst string) bool {
	if slices.Contains(s.connectors.Features(inst), manifest.FeatureThreads) {
		return false
	}
	writeErr(w, http.StatusBadRequest, "unsupported", fmt.Sprintf("connector %s has no threads", s.connectors.SlugOf(inst)))
	return true
}

// handleConnectorBind binds a room to the assistant or an agent of a
// workspace. The operator is "assistant" or an agent name.
func (s *Server) handleConnectorBind(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	room := r.PathValue("room")
	var body struct {
		Workspace string `json:"workspace"`
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
	default:
		row, err := s.findAgentRow(r.Context(), ws, operator)
		if err != nil {
			writeResolveErr(w, err, "agent")
			return
		}
		operator = row.ID
	}
	addr := conversations.Address{Connector: email.Instance, Instance: email.Instance, Room: room, Thread: body.Thread}
	if ref := r.PathValue("ref"); ref != email.Instance {
		c, err := s.connectors.Lookup(ref)
		if err != nil {
			s.writeAnyConnectorErr(w, err)
			return
		}
		if err := s.connectors.HiddenIn(c.ID, ws); err != nil {
			s.writeAnyConnectorErr(w, err)
			return
		}
		if !s.connectors.IsChannel(c.ID) {
			writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("connector %q is not a channel", c.Slug))
			return
		}
		if body.Thread != "" && s.rejectNoThreads(w, c.ID) {
			return
		}
		addr.Connector, addr.Instance = c.Type, c.ID
	}
	p, err := s.conv.BindRoom(r.Context(), ws, operator, addr)
	if s.convErr(w, err, "workspace not found") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleConnectorDependents lists the event triggers and the room bindings
// that use a connection.
func (s *Server) handleConnectorDependents(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceOf(r)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	slug := s.connectors.SlugOf(inst)
	bindings, err := s.conv.RoomBindings(r.Context(), inst)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": inst, "slug": slug,
		"triggers": s.connectors.TriggerDependents(r.Context(), slug),
		"bindings": bindings,
	})
}

// handleConnectorSenders lists the recent senders of a connection, from its
// stored events.
func (s *Server) handleConnectorSenders(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceOf(r)
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	senders, err := s.connectors.Senders(r.Context(), inst, r.URL.Query().Get("q"), parseLimit(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "senders": senders})
}

// writeSubscriptionView writes a view or the error of a subscription call.
func (s *Server) writeSubscriptionView(w http.ResponseWriter, status int, v connectors.SubscriptionView, err error) {
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, status, v)
}

// handleConnectorSubscriptions lists the subscriptions of a connection with
// the last states of its adapter.
func (s *Server) handleConnectorSubscriptions(w http.ResponseWriter, r *http.Request) {
	v, err := s.connectors.Subscriptions(r.Context(), r.PathValue("ref"))
	s.writeSubscriptionView(w, http.StatusOK, v, err)
}

func (s *Server) handleConnectorSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values map[string]connectors.Value `json:"values"`
		Types  []string                    `json:"types"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	v, err := s.connectors.Subscribe(r.Context(), r.PathValue("ref"), connectors.Subscription{Values: body.Values, Types: body.Types})
	s.writeSubscriptionView(w, http.StatusCreated, v, err)
}

func (s *Server) handleConnectorSubscriptionPatch(w http.ResponseWriter, r *http.Request) {
	var body connectors.SubscriptionPatch
	if !decodeBody(w, r, &body) {
		return
	}
	v, err := s.connectors.UpdateSubscription(r.Context(), r.PathValue("ref"), r.PathValue("id"), body)
	s.writeSubscriptionView(w, http.StatusOK, v, err)
}

func (s *Server) handleConnectorUnsubscribe(w http.ResponseWriter, r *http.Request) {
	v, err := s.connectors.Unsubscribe(r.Context(), r.PathValue("ref"), r.PathValue("id"))
	s.writeSubscriptionView(w, http.StatusOK, v, err)
}

func (s *Server) handleConnectorSubscriptionSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Fields map[string]string `json:"fields"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	v, err := s.connectors.SubmitSubscription(r.Context(), r.PathValue("ref"), r.PathValue("id"), body.Fields)
	s.writeSubscriptionView(w, http.StatusOK, v, err)
}

func (s *Server) handleConnectorSubscriptionsSync(w http.ResponseWriter, r *http.Request) {
	v, err := s.connectors.SyncSubscriptions(r.Context(), r.PathValue("ref"))
	s.writeSubscriptionView(w, http.StatusOK, v, err)
}

// handleConnectorSubscriptionOptions lists the options of a dynamic field.
// It is a POST, because the options can depend on the other values.
func (s *Server) handleConnectorSubscriptionOptions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Field  string                      `json:"field"`
		Q      string                      `json:"q"`
		Values map[string]connectors.Value `json:"values"`
		Cursor string                      `json:"cursor"`
		Limit  int                         `json:"limit"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	values := make(map[string][]string, len(body.Values))
	for k, v := range body.Values {
		values[k] = v
	}
	ref := r.PathValue("ref")
	page, err := s.connectors.SubscriptionOptions(r.Context(), ref, protocol.OptionsParams{
		Field: body.Field, Query: body.Q, Values: values, Page: body.Cursor, Limit: body.Limit,
	})
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	out := map[string]any{"id": s.connectors.IDOf(ref), "options": page.Options}
	if page.Next != "" {
		out["next_cursor"] = page.Next
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleConnectorOverrideGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	ov, found, err := s.connectors.Override(r.Context(), ws.ID, r.PathValue("ref"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "workspace has no layer for this connector")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": ws.ID, "slug": s.connectors.SlugOf(s.connectors.IDOf(r.PathValue("ref"))), "override": ov})
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
	ref := r.PathValue("ref")
	if err := s.connectors.SetOverride(r.Context(), ws.ID, ref, &ov); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	row, err := s.connectors.GetFor(ref, ws.ID)
	if err != nil {
		row = map[string]any{"ref": ref}
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
	ref := r.PathValue("ref")
	if err := s.connectors.SetOverride(r.Context(), ws.ID, ref, nil); err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": ws.ID, "deleted": ref})
}

// nonNil turns a nil slice into an empty one, so JSON shows [].
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
