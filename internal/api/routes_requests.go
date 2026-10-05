package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/tools"
)

func (s *Server) routeRequests(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/requests", s.handleRequestCreate)
	mux.HandleFunc("GET /v1/requests", s.handleRequestsList)
	mux.HandleFunc("GET /v1/requests/{id}", s.handleRequestGet)
	mux.HandleFunc("GET /v1/requests/{id}/report", s.handleRequestReport)
	mux.HandleFunc("POST /v1/requests/{id}/answer", s.handleRequestAnswer)
	mux.HandleFunc("POST /v1/requests/{id}/dismiss", s.handleRequestDismiss)
}

// reportCSP gives an HTML report an opaque origin with no network access to
// the local API (plan E.2).
const reportCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src data: https:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'"

const (
	requestNextMessage = "Request saved. End your turn now. Do not wait and do not poll. Trebi continues this session with the person's answer."
	requestPaneMessage = " Print the completion line now."
)

// requestCreateBody is the wire shape of POST /v1/requests: the payload
// fields are flat, and expires_in is a duration string.
type requestCreateBody struct {
	Type      requests.Type         `json:"type"`
	Title     string                `json:"title"`
	Summary   string                `json:"summary"`
	Target    string                `json:"target"`
	Draft     *requests.Draft       `json:"draft"`
	Options   []requests.Option     `json:"options"`
	Multi     bool                  `json:"multi"`
	Schema    json.RawMessage       `json:"schema"`
	Allow     []requests.Decision   `json:"allow"`
	Report    string                `json:"report"`
	Delivery  requests.DeliveryMode `json:"delivery"`
	ExpiresIn string                `json:"expires_in"`
}

// requireFeature writes 402 when the cloud plan does not grant f.
func (s *Server) requireFeature(w http.ResponseWriter, f entitle.Feature) bool {
	if s.cloud != nil && s.cloud.Entitlement().Has(f) {
		return true
	}
	writePlanRequired(w, f)
	return false
}

func writePlanRequired(w http.ResponseWriter, f entitle.Feature) {
	writeErrDetails(w, http.StatusPaymentRequired, "plan_required", "Requests need Trebi Pro.", map[string]string{"feature": string(f)})
}

func (s *Server) handleRequestCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := tools.ActorFrom(r.Context())
	if !ok || actor.RunID == "" {
		writeForbidden(w, requests.ErrNotRun.Error())
		return
	}
	if !s.requireFeature(w, entitle.Requests) {
		return
	}
	var body requestCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	var exp time.Duration
	if body.ExpiresIn != "" {
		d, err := time.ParseDuration(body.ExpiresIn)
		if err != nil || d <= 0 {
			writeErrDetails(w, http.StatusBadRequest, "validation", "expires_in must be a duration such as 24h", map[string]string{"field": "expires_in"})
			return
		}
		exp = d
	}
	req, err := s.requests.Create(r.Context(), actor, requests.CreateOpts{
		Type: body.Type, Title: body.Title, Summary: body.Summary, Target: body.Target,
		Payload: requests.Payload{Draft: body.Draft, Options: body.Options, Multi: body.Multi, Schema: body.Schema},
		Allow:   body.Allow, Report: body.Report, Delivery: body.Delivery, ExpiresIn: exp,
	})
	if err != nil {
		writeRequestErr(w, req, err)
		return
	}
	out := map[string]any{"request": req}
	if req.Delivery == requests.DeliveryResume {
		msg := requestNextMessage
		if req.Runtime == "tui" {
			msg += requestPaneMessage
		}
		out["next"], out["message"] = "end_turn", msg
	}
	writeJSON(w, http.StatusCreated, out)
}

// requestScope limits a run token to the requests of its own job.
func requestScope(r *http.Request) (workspace, job string, scoped bool) {
	a, ok := tools.ActorFrom(r.Context())
	if !ok || a.RunID == "" {
		return "", "", false
	}
	return a.WorkspaceID, a.Job, true
}

func (s *Server) handleRequestsList(w http.ResponseWriter, r *http.Request) {
	after, ok := parseULIDCursor(w, r, "request")
	if !ok {
		return
	}
	q := r.URL.Query()
	o := requests.ListOpts{
		Status: requests.Status(q.Get("status")), RunID: q.Get("run"), Job: q.Get("job"),
		Workspace: q.Get("workspace"), After: after, Limit: parseLimit(r),
	}
	if ws, job, scoped := requestScope(r); scoped {
		o.Workspace, o.Job = ws, job
	}
	list, next, err := s.requests.List(r.Context(), o)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := map[string]any{"requests": list}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

// requestFor loads the request in the path and applies the run token scope.
func (s *Server) requestFor(w http.ResponseWriter, r *http.Request) (requests.Request, bool) {
	req, err := s.requests.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRequestErr(w, req, err)
		return req, false
	}
	if ws, job, scoped := requestScope(r); scoped && (req.WorkspaceID != ws || req.Job != job) {
		writeErr(w, http.StatusNotFound, "not_found", requests.ErrNotFound.Error())
		return req, false
	}
	return req, true
}

func (s *Server) handleRequestGet(w http.ResponseWriter, r *http.Request) {
	req, ok := s.requestFor(w, r)
	if !ok {
		return
	}
	if v := r.URL.Query().Get("wait"); v != "" && req.Open() {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			writeErr(w, http.StatusBadRequest, "validation", "wait must be a duration such as 30s")
			return
		}
		if req, err = s.requests.Wait(r.Context(), req.ID, d); err != nil {
			writeRequestErr(w, req, err)
			return
		}
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(req.Revision)+`"`)
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleRequestReport(w http.ResponseWriter, r *http.Request) {
	req, ok := s.requestFor(w, r)
	if !ok {
		return
	}
	body, err := s.requests.Reports().Open(req.ID, req.ReportKind)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "request has no report")
		return
	}
	defer body.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if req.ReportKind == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", reportCSP)
	} else {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body) // the client went away; nothing to report
}

// answerer returns who answers and through which surface. A human caller
// may name cli; everything else is ui.
func answerer(r *http.Request, via string) (by, surface string) {
	if a, ok := tools.ActorFrom(r.Context()); ok && a.Kind == "assistant" {
		return requests.ByAssistant, requests.ViaAsst
	}
	if via == requests.ViaCLI {
		return requests.ByUser, requests.ViaCLI
	}
	return requests.ByUser, requests.ViaUI
}

func (s *Server) handleRequestAnswer(w http.ResponseWriter, r *http.Request) {
	if rejectAnyRunToken(w, r) {
		return
	}
	var body struct {
		requests.Answer
		Via string `json:"via"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	rev := 0
	if v := ifMatchHeader(r); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "If-Match must be a request revision")
			return
		}
		rev = n
	}
	a := body.Answer
	a.By, a.Via = answerer(r, body.Via)
	req, err := s.requests.Answer(r.Context(), r.PathValue("id"), a, rev)
	if err != nil {
		writeRequestErr(w, req, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleRequestDismiss(w http.ResponseWriter, r *http.Request) {
	if rejectAnyRunToken(w, r) {
		return
	}
	by, _ := answerer(r, "")
	req, err := s.requests.Dismiss(r.Context(), r.PathValue("id"), by)
	if err != nil {
		writeRequestErr(w, req, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func writeRequestErr(w http.ResponseWriter, req requests.Request, err error) {
	var fe *requests.FieldError
	switch {
	case errors.As(err, &fe):
		writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), map[string]string{"field": fe.Field})
	case errors.Is(err, requests.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, requests.ErrClosed):
		writeErrDetails(w, http.StatusConflict, "request_closed", err.Error(), map[string]any{"request": req})
	case errors.Is(err, requests.ErrConflict):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.Is(err, requests.ErrNotRun), errors.Is(err, requests.ErrRunToken):
		writeForbidden(w, err.Error())
	case errors.Is(err, requests.ErrPlan):
		writePlanRequired(w, entitle.Requests)
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
