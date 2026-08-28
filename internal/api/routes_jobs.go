package api

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
)

func (s *Server) routeJobs(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/jobs", s.handleJobsList)
	mux.HandleFunc("POST /v1/jobs", s.handleJobsCreate)
	mux.HandleFunc("GET /v1/jobs/{name}", s.handleJobGet)
	mux.HandleFunc("PUT /v1/jobs/{name}", s.handleJobPut)
	mux.HandleFunc("PATCH /v1/jobs/{name}", s.handleJobPatch)
	mux.HandleFunc("DELETE /v1/jobs/{name}", s.handleJobDelete)
	mux.HandleFunc("POST /v1/jobs/{name}/run", s.handleJobRun)
	mux.HandleFunc("POST /v1/jobs/{name}/pause", s.handleJobPause)
	mux.HandleFunc("POST /v1/jobs/{name}/unpause", s.handleJobUnpause)
	mux.HandleFunc("POST /v1/jobs/{name}/validate", s.handleJobValidate)
	mux.HandleFunc("GET /v1/jobs/{name}/memory", s.handleJobMemoryGet)
	mux.HandleFunc("PUT /v1/jobs/{name}/memory", s.handleJobMemoryPut)
	mux.HandleFunc("DELETE /v1/jobs/{name}/memory", func(w http.ResponseWriter, r *http.Request) {
		s.handleMemoryDelete(w, r, false)
	})
	mux.HandleFunc("POST /v1/jobs/{name}/memory/entries", func(w http.ResponseWriter, r *http.Request) {
		s.handleMemoryEntriesPost(w, r, false)
	})
	mux.HandleFunc("DELETE /v1/jobs/{name}/memory/entries/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handleMemoryEntriesDelete(w, r, false)
	})
	mux.HandleFunc("GET /v1/schedules", s.handleSchedules)
}

func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("workspace")
	rows, err := s.ws.ListJobs(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	last, live, err := s.runStatusByJob(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// B4: the assistant is a separate singleton; jobs list is jobs only.
		if row.Kind != "job" {
			continue
		}
		key := row.WorkspaceID + "/" + row.Name
		waiting := false
		for _, id := range live[key] {
			if s.runs.AgentState(id) == runs.AgentWaiting {
				waiting = true
				break
			}
		}
		status := runs.DeriveJobDisplayStatus(row.Active, len(live[key]), waiting, last[key])
		out = append(out, map[string]any{
			"workspace_id": row.WorkspaceID, "name": row.Name, "path": row.Path,
			"kind": row.Kind, "active": row.Active, "harness": row.Harness,
			"hash": row.Hash, "parse_error": row.ParseError, "status": status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// runStatusByJob returns the newest run status and live run ids per
// "workspace/job" lane (kind=job).
func (s *Server) runStatusByJob(ctx context.Context) (map[string]runs.Status, map[string][]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT workspace_id, job, id, status FROM runs WHERE kind='job' ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	last := map[string]runs.Status{}
	live := map[string][]string{}
	for rows.Next() {
		var ws, jb, id, st string
		if err := rows.Scan(&ws, &jb, &id, &st); err != nil {
			return nil, nil, err
		}
		key := ws + "/" + jb
		last[key] = runs.Status(st) // ULID order: the final row is the newest
		if runs.Status(st).IsLive() {
			live[key] = append(live[key], id)
		}
	}
	return last, live, rows.Err()
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	j, err := job.ParseFile(row.Path)
	if err != nil && j == nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data, _ := os.ReadFile(row.Path)
	// If-Match ETag is full content hash (not the short parse index hash).
	hash := job.ContentHash(data)
	var mtime string
	if st, err := os.Stat(row.Path); err == nil {
		mtime = st.ModTime().UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": j.Name, "workspace": row.WorkspaceID, "path": row.Path,
		"harness": j.Harness, "active": !j.Disabled, "hash": hash,
		"parse_error": j.ParseError, "prompt": j.Prompt,
		"triggers": j.Triggers, "session": j.Session, "env_set": j.Envset,
		"markdown": string(data), "mtime": mtime,
	})
}

func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request) {
	s.createRunFromJob(w, r, r.PathValue("name"))
}

func (s *Server) handleJobPause(w http.ResponseWriter, r *http.Request) {
	s.setJobDisabled(w, r, true)
}

func (s *Server) handleJobUnpause(w http.ResponseWriter, r *http.Request) {
	s.setJobDisabled(w, r, false)
}

func (s *Server) setJobDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	if err := job.SetDisabled(row.Path, disabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": row.Name, "disabled": disabled})
}

func (s *Server) handleJobValidate(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	j, err := job.ParseFile(row.Path)
	valid := err == nil && j != nil && j.ParseError == ""
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if j != nil {
		msg = j.ParseError
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": valid, "error": msg})
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	// Name-keyed collection: limit only, no ULID cursor.
	rows, err := s.st.DB().QueryContext(r.Context(), `SELECT workspace_id, job, idx, spec, timezone, catch_up, next_due_at, last_fired_at FROM schedules ORDER BY next_due_at LIMIT ?`, parseLimit(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	var list []map[string]any
	for rows.Next() {
		var ws, jobName, spec, tz string
		var idx, catch int
		var next, last sql.NullString
		if err := rows.Scan(&ws, &jobName, &idx, &spec, &tz, &catch, &next, &last); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		list = append(list, map[string]any{
			"workspace": ws, "job": jobName, "idx": idx, "spec": spec,
			"timezone": tz, "catch_up": catch == 1,
			"next_due_at": next.String, "last_fired_at": last.String,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": list})
}
