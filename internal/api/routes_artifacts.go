package api

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/artifacts"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tools"
)

func (s *Server) routeArtifacts(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/artifacts", s.handleArtifactList)
	mux.HandleFunc("POST /v1/artifacts", s.handleArtifactPut)
	mux.HandleFunc("GET /v1/artifacts/{id}", s.handleArtifactGet)
	mux.HandleFunc("PATCH /v1/artifacts/{id}", s.handleArtifactPatch)
	mux.HandleFunc("DELETE /v1/artifacts/{id}", s.handleArtifactDelete)
	mux.HandleFunc("GET /v1/artifacts/{id}/versions", s.handleArtifactVersions)
	mux.HandleFunc("GET /v1/artifacts/{id}/versions/{n}", s.handleArtifactVersion)
	mux.HandleFunc("GET /v1/artifacts/{id}/versions/{n}/content", s.handleArtifactContent)
	mux.HandleFunc("POST /v1/artifacts/{id}/versions/{n}/pin", s.handleArtifactPin)
	mux.HandleFunc("DELETE /v1/artifacts/{id}/versions/{n}/pin", s.handleArtifactPin)
	mux.HandleFunc("POST /v1/artifacts/{id}/versions/{n}/send", s.handleArtifactSend)
	mux.HandleFunc("GET /v1/artifacts/{id}/deliveries", s.handleArtifactDeliveries)
	mux.HandleFunc("POST /v1/artifacts/{id}/ask", s.handleArtifactAsk)
	mux.HandleFunc("GET /v1/runs/{id}/artifacts", s.handleRunArtifacts)
}

// artifactBodyCap fits the largest artifact as base64 plus the JSON fields.
const artifactBodyCap = artifacts.DefaultMaxBytes*4/3 + 1<<20

// artifactCSP sandboxes every content response (D19).
const artifactCSP = "sandbox allow-scripts; default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; font-src data:"

// inputError is a bad request that the handler finds after the decode.
type inputError string

func (e inputError) Error() string { return string(e) }

func writeArtifactErr(w http.ResponseWriter, err error) {
	var stale *artifacts.StaleError
	var inv *artifacts.InvalidError
	var in inputError
	switch {
	case errors.As(err, &in):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.As(err, &stale):
		writeErrDetails(w, http.StatusConflict, "stale", err.Error(), map[string]any{"latest": stale.Latest})
	case errors.As(err, &inv):
		writeErrDetails(w, http.StatusUnprocessableEntity, "invalid", err.Error(), map[string]any{"fields": inv.Fields})
	case errors.Is(err, artifacts.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, artifacts.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, artifacts.ErrAmbiguous):
		writeErr(w, http.StatusConflict, "ambiguous", err.Error())
	case errors.Is(err, artifacts.ErrConflict):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
	case errors.Is(err, artifacts.ErrBadCursor):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// artifactActor returns the caller, or nil for a person.
func artifactActor(r *http.Request) *tools.Actor {
	if a, ok := tools.ActorFrom(r.Context()); ok {
		return &a
	}
	return nil
}

// artifactWorkspace resolves the workspace of a call. A run or an assistant
// session uses only its own workspace.
func (s *Server) artifactWorkspace(w http.ResponseWriter, r *http.Request, ref string) (string, bool) {
	a := artifactActor(r)
	if strings.TrimSpace(ref) == "" {
		if a != nil && a.WorkspaceID != "" {
			return a.WorkspaceID, true
		}
		return "", true
	}
	ws, err := s.ws.Resolve(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return "", false
	}
	if a != nil && a.WorkspaceID != "" && a.WorkspaceID != ws.ID {
		writeForbidden(w, "a run may use only the artifacts of its own workspace")
		return "", false
	}
	return ws.ID, true
}

// artifactFor resolves the {id} path value: an id, or a name in the
// workspace of the caller or of ?workspace=.
func (s *Server) artifactFor(w http.ResponseWriter, r *http.Request) (artifacts.Artifact, bool) {
	ref := r.PathValue("id")
	var a artifacts.Artifact
	var err error
	if strings.HasPrefix(ref, "art_") {
		a, err = s.artifacts.Get(r.Context(), ref)
	} else {
		ws, ok := s.artifactWorkspace(w, r, r.URL.Query().Get("workspace"))
		if !ok {
			return a, false
		}
		if ws == "" {
			writeErr(w, http.StatusBadRequest, "validation", "workspace is required to find an artifact by name")
			return a, false
		}
		var owner artifacts.Origin
		if owner, err = s.artifacts.OriginFor(r.Context(), artifactActor(r), "", ""); err == nil {
			a, _, err = s.artifacts.Resolve(r.Context(), ws, owner, ref)
		}
	}
	if err != nil {
		writeArtifactErr(w, err)
		return a, false
	}
	if act := artifactActor(r); act != nil && act.WorkspaceID != "" && act.WorkspaceID != a.Workspace {
		writeErr(w, http.StatusNotFound, "not_found", artifacts.ErrNotFound.Error())
		return a, false
	}
	return a, true
}

// versionFor reads {n}: a number, or "latest".
func versionFor(w http.ResponseWriter, r *http.Request, a artifacts.Artifact) (int, bool) {
	raw := r.PathValue("n")
	if raw == "latest" {
		return a.LatestVersion, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		writeErr(w, http.StatusBadRequest, "validation", "version must be a number from 1, or latest")
		return 0, false
	}
	return n, true
}

// parseOwner reads "assistant", "job:<ref>", or "agent:<ref>".
func (s *Server) parseOwner(r *http.Request, ws, owner string) (kind, id string, err error) {
	kind, ref, _ := strings.Cut(strings.TrimSpace(owner), ":")
	switch kind {
	case "":
		return "", "", nil
	case artifacts.OwnerAssistant:
		return kind, "", nil
	case artifacts.OwnerJob, artifacts.OwnerAgent:
		if ref == "" {
			return "", "", errors.New("owner must be assistant, job:<name>, or agent:<name>")
		}
		if kind == artifacts.OwnerAgent && !strings.HasPrefix(ref, "agents/") && !strings.HasPrefix(ref, "job_") {
			ref = "agents/" + ref
		}
		row, err := s.ws.ResolveJob(r.Context(), ws, ref)
		if err != nil {
			return "", "", errors.New("owner " + owner + " not found")
		}
		return kind, row.ID, nil
	}
	return "", "", errors.New("owner must be assistant, job:<name>, or agent:<name>")
}

func (s *Server) handleArtifactList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ws, ok := s.artifactWorkspace(w, r, q.Get("workspace"))
	if !ok {
		return
	}
	f := artifacts.ListFilter{Workspace: ws, Kind: artifacts.Kind(q.Get("kind")), Query: q.Get("q"),
		Cursor: q.Get("cursor"), Limit: parseLimit(r)}
	if v := q.Get("live"); v != "" {
		live := v == "1" || v == "true"
		f.Live = &live
	}
	if o := q.Get("owner"); o != "" {
		var err error
		if f.OwnerKind, f.OwnerID, err = s.parseOwner(r, ws, o); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	list, next, err := s.artifacts.List(r.Context(), f)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "next_cursor": next})
}

type artifactPutBody struct {
	Workspace     string `json:"workspace"`
	Name          string `json:"name"`
	ArtifactID    string `json:"artifact_id"`
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	FileName      string `json:"file_name"`
	Content       string `json:"content"`
	ContentBase64 string `json:"content_base64"`
	FromPath      string `json:"from_path"`
	Note          string `json:"note"`
	Base          *int   `json:"base"`
	Owner         string `json:"owner"`
}

func (s *Server) handleArtifactPut(w http.ResponseWriter, r *http.Request) {
	var body artifactPutBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, artifactBodyCap)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	actor := artifactActor(r)
	ws, ok := s.artifactWorkspace(w, r, body.Workspace)
	if !ok {
		return
	}
	if ws == "" {
		writeErr(w, http.StatusBadRequest, "validation", "workspace is required")
		return
	}
	o, err := s.artifactPutOpts(r, ws, actor, body)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	v, err := s.artifactSave(r, actor, o, body)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	a, err := s.artifacts.Get(r.Context(), v.ArtifactID)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": a, "version": v})
}

// artifactPutOpts finds the origin and the target artifact of a save.
func (s *Server) artifactPutOpts(r *http.Request, ws string, actor *tools.Actor, body artifactPutBody) (artifacts.PutOpts, error) {
	person := actor == nil || actor.RunID == ""
	var ownerKind, ownerID string
	if person {
		var err error
		if ownerKind, ownerID, err = s.parseOwner(r, ws, body.Owner); err != nil {
			return artifacts.PutOpts{}, inputError(err.Error())
		}
	}
	origin, err := s.artifacts.OriginFor(r.Context(), actor, ownerKind, ownerID)
	if err != nil {
		return artifacts.PutOpts{}, err
	}
	o := artifacts.PutOpts{Workspace: ws, ArtifactID: body.ArtifactID, Name: body.Name, Title: body.Title,
		Kind: artifacts.Kind(body.Kind), FileName: body.FileName, Note: body.Note, Base: body.Base, Origin: origin}
	if o.ArtifactID == "" && body.Owner == "" && person {
		// A person who saves by name saves to the artifact of that name.
		a, _, err := s.artifacts.Resolve(r.Context(), ws, artifacts.Origin{}, body.Name)
		switch {
		case err == nil:
			o.ArtifactID = a.ID
		case !errors.Is(err, artifacts.ErrNotFound):
			return o, err
		}
	}
	if o.ArtifactID != "" {
		a, err := s.artifacts.Get(r.Context(), o.ArtifactID)
		if err == nil && a.Workspace != ws {
			err = artifacts.ErrNotFound
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

// artifactSave saves a file of the run folder, or the content of the body.
func (s *Server) artifactSave(r *http.Request, actor *tools.Actor, o artifacts.PutOpts, body artifactPutBody) (artifacts.Version, error) {
	if body.FromPath != "" {
		if actor == nil || actor.RunID == "" {
			return artifacts.Version{}, inputError("from_path is accepted only from a run; send content")
		}
		run, err := s.runs.Get(r.Context(), actor.RunID)
		if err != nil {
			return artifacts.Version{}, err
		}
		return s.artifacts.PutFromFile(r.Context(), o, run.WorkingDir, body.FromPath)
	}
	data := []byte(body.Content)
	if body.ContentBase64 != "" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(body.ContentBase64); err != nil {
			return artifacts.Version{}, inputError("content_base64 is not valid base64")
		}
	}
	o.Content = bytes.NewReader(data)
	return s.artifacts.Put(r.Context(), o)
}

func (s *Server) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.artifactFor(w, r); ok {
		writeJSON(w, http.StatusOK, a)
	}
}

func (s *Server) handleArtifactPatch(w http.ResponseWriter, r *http.Request) {
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	var p artifacts.Patch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	var rev int64
	if m := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); m != "" {
		var err error
		if rev, err = strconv.ParseInt(m, 10, 64); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "If-Match must be the artifact revision")
			return
		}
	}
	out, err := s.artifacts.Update(r.Context(), a.ID, p, rev)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleArtifactDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	if err := s.artifacts.Delete(r.Context(), a.ID); err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": a.ID})
}

func (s *Server) handleArtifactVersions(w http.ResponseWriter, r *http.Request) {
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	list, err := s.artifacts.Versions(r.Context(), a.ID)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleArtifactVersion(w http.ResponseWriter, r *http.Request) {
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	n, ok := versionFor(w, r, a)
	if !ok {
		return
	}
	v, err := s.artifacts.Version(r.Context(), a.ID, n)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleArtifactContent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	n, ok := versionFor(w, r, a)
	if !ok {
		return
	}
	rc, v, err := s.artifacts.Open(r.Context(), a.ID, n)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	etag := `"` + v.SHA256 + `"`
	h.Set("ETag", etag)
	h.Set("Content-Security-Policy", artifactCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=0, must-revalidate")
	if r.URL.Query().Get("download") == "1" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": v.FileName}))
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", v.MIME)
	h.Set("Content-Length", strconv.FormatInt(v.SizeBytes, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc) // the client went away
	}
}

func (s *Server) handleArtifactPin(w http.ResponseWriter, r *http.Request) {
	if a := artifactActor(r); a != nil && a.RunID != "" {
		writeForbidden(w, "only a person can pin a version")
		return
	}
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	n, ok := versionFor(w, r, a)
	if !ok {
		return
	}
	on := r.Method == http.MethodPost
	if err := s.artifacts.Pin(r.Context(), a.ID, n, on); err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact_id": a.ID, "n": n, "pinned": on})
}

func (s *Server) handleArtifactDeliveries(w http.ResponseWriter, r *http.Request) {
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	list, err := s.artifacts.Deliveries(r.Context(), a.ID)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleArtifactAsk(w http.ResponseWriter, r *http.Request) {
	if rejectAnyRunToken(w, r) {
		return
	}
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Text      string `json:"text"`
		Version   int    `json:"version"`
		Selection string `json:"selection"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	res, err := s.artifacts.Ask(r.Context(), artifacts.AskOpts{ID: a.ID, Version: body.Version, Text: body.Text, Selection: body.Selection})
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleRunArtifacts(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if a := artifactActor(r); a != nil && a.WorkspaceID != "" && a.WorkspaceID != run.Workspace {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	list, err := s.artifacts.ByRun(r.Context(), run.ID)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// artifactURL is the UI page of a version, for the link in a mail.
func artifactURL(id string, n int) string {
	base := home.APIJSONURL()
	if cfg, err := config.Load(); err == nil && cfg != nil && cfg.UI.PublicURL != "" {
		base = cfg.UI.PublicURL
	}
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/artifacts/%s?v=%d", strings.TrimRight(base, "/"), id, n)
}

// handleArtifactSend sends an email draft (C3): through the native channel
// for from: trebi, else through the send or draft operation of the
// mailbox connection.
func (s *Server) handleArtifactSend(w http.ResponseWriter, r *http.Request) {
	a, ok := s.artifactFor(w, r)
	if !ok {
		return
	}
	n, ok := versionFor(w, r, a)
	if !ok {
		return
	}
	var body struct {
		Mode string `json:"mode"` // send | draft
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Mode == "" {
		body.Mode = "send"
	}
	if body.Mode != "send" && body.Mode != "draft" {
		writeErr(w, http.StatusBadRequest, "validation", "mode must be send or draft")
		return
	}
	if a.Kind != artifacts.KindEmail {
		writeArtifactErr(w, &artifacts.InvalidError{Fields: []artifacts.FieldError{{Field: "kind",
			Message: "only an email draft can be sent; use trebi email send --artifact for a report"}}})
		return
	}
	data, _, v, err := s.artifacts.Read(r.Context(), a.ID, n)
	if err != nil {
		writeArtifactErr(w, err)
		return
	}
	d, errs := artifacts.ParseDraft(data)
	if len(errs) > 0 {
		writeArtifactErr(w, &artifacts.InvalidError{Fields: errs})
		return
	}
	actor := artifactActor(r)
	byUser := actor == nil || actor.Kind == tools.ActorMobile
	ref := &email.ArtifactRef{ID: a.ID, Version: v.N, URL: artifactURL(a.ID, v.N)}
	if d.From == artifacts.FromTrebi {
		s.sendDraftNative(w, r, a, d, ref, byUser)
		return
	}
	s.sendDraftConnection(w, r, a, d, ref, byUser, body.Mode)
}

// sendDraftConnection sends or saves a draft through the mailbox connection of d.From.
func (s *Server) sendDraftConnection(w http.ResponseWriter, r *http.Request, a artifacts.Artifact, d artifacts.Draft, ref *email.ArtifactRef, byUser bool, mode string) {
	mimeBytes, msgID, err := s.email.BuildMIME(email.Draft{To: d.To, Cc: d.Cc, Subject: d.Subject, InReplyTo: d.InReplyTo, Text: d.Body})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	c := connectors.CallOpts{Workspace: a.Workspace, Connection: d.From, Operation: mode,
		Input: map[string]any{"mime_base64": base64.StdEncoding.EncodeToString(mimeBytes)}, Agent: !byUser}
	if actor := artifactActor(r); actor != nil {
		c.RunID = actor.RunID
	}
	res, err := s.operations().Call(r.Context(), c)
	if errors.Is(err, connectors.ErrOperationOff) && mode == "send" {
		writeErr(w, http.StatusConflict, "operation_off",
			"Sending from this account is off. Turn it on in the connection settings, or save it to your drafts.")
		return
	}
	if err != nil {
		writeOperationErr(w, s, err)
		return
	}
	out := map[string]any{"via": "connection", "connection": d.From, "mode": mode, "result": res}
	if mode == "send" {
		var sent struct {
			MessageID string `json:"message_id"`
		}
		_ = json.Unmarshal(res.Output, &sent) // a program that prints no id keeps the minted one
		rowID, err := s.email.RecordSent(r.Context(), email.SentByConnector{WorkspaceID: a.Workspace,
			MessageID: cmp.Or(sent.MessageID, msgID), InReplyTo: d.InReplyTo, To: append(slices.Clone(d.To), d.Cc...),
			Subject: d.Subject, RunID: c.RunID, Artifact: ref})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out["email_id"] = rowID
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) sendDraftNative(w http.ResponseWriter, r *http.Request, a artifacts.Artifact, d artifacts.Draft, ref *email.ArtifactRef, byUser bool) {
	ws, err := s.ws.Resolve(r.Context(), a.Workspace)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if s.email.Configured() {
		if _, err := s.email.EnsureAddress(r.Context(), ws.ID, ws.Handle); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	// The native channel has no Cc header; Cc joins To.
	opts := email.SendOpts{WorkspaceID: ws.ID, To: append(slices.Clone(d.To), d.Cc...), Subject: d.Subject,
		InReplyTo: d.InReplyTo, Artifact: ref, ByUser: byUser}
	if actor := artifactActor(r); actor != nil {
		opts.RunID, opts.ConversationID = actor.RunID, actor.Conversation
	}
	if a.OwnerKind == artifacts.OwnerAgent {
		if row, err := s.ws.JobByID(r.Context(), a.OwnerID); err == nil {
			opts.Agent = strings.TrimPrefix(row.Name, "agents/")
			opts.FromName = opts.Agent
		}
	}
	q, err := s.email.Send(r.Context(), opts)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	row, err := s.outbox.SendNow(r.Context(), q.OutboxID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"via": "trebi", "message_id": q.MessageID, "outbox_id": row.ID,
		"status": row.Status, "sent": row.Status == outbox.StatusSent})
}

// artContinuer sends the request of an artifact Ask to its origin.
type artContinuer struct{ s *Server }

func (c artContinuer) SendToConversation(ctx context.Context, convID, text string) error {
	_, err := c.s.conv.Send(ctx, convID, conversations.SendOpts{Text: text})
	return err
}

func (c artContinuer) ResumeRun(ctx context.Context, runID, prompt string) (string, error) {
	run, err := c.s.launch.Resume(ctx, runID, runs.ResumeOpts{Reason: runs.ReasonMessage, Prompt: prompt})
	if err != nil {
		return "", err
	}
	return run.ID, nil
}

func (c artContinuer) AskAssistant(ctx context.Context, workspaceID, text string) (string, error) {
	out, err := c.s.conv.Ask(ctx, conversations.AskOpts{Workspace: workspaceID, Text: text})
	if err != nil {
		return "", err
	}
	return out.ConversationID, nil
}
