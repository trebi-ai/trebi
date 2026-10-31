package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"strings"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/machines"
	"github.com/trebi-ai/trebi/internal/relay"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/visor"
)

func (s *Server) routeMachines(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/machines", s.handleMachineList)
	mux.HandleFunc("GET /v1/machines/{id}", s.handleMachineGet)
	mux.HandleFunc("POST /v1/machines/{id}/link", s.handleMachineLink)
	mux.HandleFunc("DELETE /v1/machines/{id}/link", s.handleMachineUnlink)
	mux.HandleFunc("GET /v1/machines/{id}/sessions", s.handleObservedList)
	mux.HandleFunc("GET /v1/machines/{id}/sessions/{session}", s.handleObservedGet)
}

// bindMachines builds the machines service over the current harness,
// connector, and cloud services.
func (s *Server) bindMachines() {
	s.machines = machines.New(s.st, s.harness, agentcli.HarnessLocator{}, s.connectors, s.cloud).WithVersion(Version).WithLinks(s.links)
}

// BindLinks swaps in the daemon's peer links, so the routes and the
// forwarded requests share one session per machine.
func (s *Server) BindLinks(l *machines.Links) {
	bind(&s.links, l)
	s.bindMachines()
}

// readiness checks what a job or an agent needs on the machine of its
// workspace. A failed check gives an empty list; it never fails the read.
func (s *Server) readiness(ctx context.Context, wsID string, j *job.Job, kit *job.KitYAML) []machines.Item {
	items, err := s.machines.Readiness(ctx, wsID, machines.NeedsOf(j, kit))
	if err != nil {
		s.log.WarnContext(ctx, "machine.readiness", "err", err, "workspace_id", wsID)
		return []machines.Item{}
	}
	return items
}

// handleMachineList returns the machine roster. It is short, so it has no
// pages.
func (s *Server) handleMachineList(w http.ResponseWriter, r *http.Request) {
	list, err := s.machines.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": list})
}

// handleMachineGet returns one machine. The id "self" is this machine. The
// self row also has its capabilities.
func (s *Server) handleMachineGet(w http.ResponseWriter, r *http.Request) {
	m, err := s.machines.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, machines.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if m.IsSelf {
		caps, err := s.machines.Capabilities(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		caps.Versions = machines.Versions{Trebi: Version, Edition: s.ext.EditionVersion, UI: s.uiVersion()}
		m.Capabilities = &caps
	}
	writeJSON(w, http.StatusOK, m)
}

// handleMachineLink connects this machine to another machine of the
// account with the pairing link that the other machine made.
func (s *Server) handleMachineLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link string `json:"link"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	m, ok := s.remoteMachine(w, r)
	if !ok {
		return
	}
	if err := s.links.Connect(r.Context(), m, body.Link); err != nil {
		writeLinkErr(w, err)
		return
	}
	s.handleMachineGet(w, r)
}

// handleMachineUnlink removes the link to another machine.
func (s *Server) handleMachineUnlink(w http.ResponseWriter, r *http.Request) {
	m, ok := s.remoteMachine(w, r)
	if !ok {
		return
	}
	if err := s.links.Disconnect(r.Context(), m); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// remoteMachine reads the machine of the path. It writes an error and
// gives false when the id is unknown or is this machine.
func (s *Server) remoteMachine(w http.ResponseWriter, r *http.Request) (machines.Machine, bool) {
	m, err := s.machines.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, machines.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	case m.IsSelf:
		writeErr(w, http.StatusBadRequest, "validation", "This is the machine you use now.")
	default:
		return m, true
	}
	return m, false
}

func writeLinkErr(w http.ResponseWriter, err error) {
	var apiErr *cloudlink.APIError
	switch {
	case errors.Is(err, machines.ErrBadLink):
		msg := strings.TrimPrefix(err.Error(), machines.ErrBadLink.Error()+": ") // never empty
		writeErr(w, http.StatusBadRequest, "validation", strings.ToUpper(msg[:1])+msg[1:]+".")
	case errors.Is(err, machines.ErrLinkUsed):
		writeErr(w, http.StatusConflict, "conflict", "The link was already used or has expired. Make a new link on the other machine.")
	case errors.Is(err, cloudlink.ErrNotLinked):
		writeErr(w, http.StatusConflict, "not_linked", "Link this machine to Trebi Cloud first.")
	case errors.As(err, &apiErr) && apiErr.Code == "remote_access_required":
		writeErr(w, http.StatusConflict, "remote_access_required", "Connecting machines needs Trebi Pro Cloud.")
	default:
		writeErr(w, http.StatusBadGateway, "unavailable", "The other machine did not answer: "+err.Error())
	}
}

// forwardMachine sends a request for another machine through the peer link
// (relay.HeaderMachine). Only a person at the UI or a phone can use another
// machine; a request of an agent never leaves this machine.
func (s *Server) forwardMachine(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := relay.MachineTarget(r)
		if target == "" || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		if a, ok := tools.ActorFrom(r.Context()); (ok && a.Kind != tools.ActorMobile) || r.Header.Get(tools.HeaderRunToken) != "" {
			writeErr(w, http.StatusBadRequest, "validation", "an agent cannot use another machine")
			return
		}
		m, err := s.machines.Get(r.Context(), target)
		switch {
		case errors.Is(err, machines.ErrNotFound):
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		case m.IsSelf:
			relay.StripMachineTarget(r)
			next.ServeHTTP(w, r)
			return
		}
		peer, err := s.links.Peer(r.Context(), m.ID)
		if errors.Is(err, machines.ErrNoLink) {
			writeErr(w, http.StatusConflict, "not_connected", "Connect to "+m.Name+" first.")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "peer"
				relay.StripMachineTarget(pr.Out)
				pr.Out.Header.Del(tools.HeaderActor)
			},
			Transport:     peer.Transport(),
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				writeErr(w, http.StatusBadGateway, "unavailable", m.Name+" did not answer: "+err.Error())
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

// observed gives the sessions on this machine that started outside Trebi.
func (s *Server) observed() *visor.Observed { return visor.NewObserved(s.runs.SessionHost) }

// selfMachine writes an error and gives false when the id is not this
// machine. Only this machine sees its own sessions.
func (s *Server) selfMachine(w http.ResponseWriter, r *http.Request) bool {
	m, err := s.machines.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, machines.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	case !m.IsSelf:
		writeErr(w, http.StatusConflict, "remote_machine", "Open Trebi on "+m.Name+" to see its sessions.")
	default:
		return true
	}
	return false
}

// handleObservedList returns the agent sessions that the user started
// outside Trebi on this machine.
func (s *Server) handleObservedList(w http.ResponseWriter, r *http.Request) {
	if !s.selfMachine(w, r) {
		return
	}
	list, err := s.observed().List(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

// handleObservedGet returns one observed session with its turns.
func (s *Server) handleObservedGet(w http.ResponseWriter, r *http.Request) {
	if !s.selfMachine(w, r) {
		return
	}
	sess, turns, err := s.observed().Get(r.Context(), r.PathValue("session"))
	switch {
	case errors.Is(err, agentvisor.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "no session "+r.PathValue("session"))
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"session": sess, "turns": turns})
	}
}
