package requests

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const requestColumns = `id, run_id, workspace_id, job_id,
  COALESCE((SELECT ji.name FROM job_index ji WHERE ji.id = requests.job_id), ''), kind, session_id, harness, runtime,
  working_dir, conversation_key, conversation_id, title, summary, turn, status,
  answered_by, answered_via, answered_at, expires_at, delivery_state, claimed_at, delivery_path,
  delivered_run_id, delivered_turn, delivery_error, source, hop_id, trace_id, created_at, updated_at, revision`

const formColumns = `request_id, idx, type, title, summary, target, payload_json, allow_json,
  report_kind, report_sha256, answer_json`

type rowScanner interface{ Scan(dest ...any) error }

func scanRequest(rs rowScanner) (Request, error) {
	var r Request
	var createdAt, updatedAt string
	var by, via, answeredAt, expiresAt, claimedAt, deliveredRun sql.NullString
	var deliveredTurn sql.NullInt64
	if err := rs.Scan(&r.ID, &r.RunID, &r.WorkspaceID, &r.JobID, &r.JobName, &r.Kind, &r.SessionID,
		&r.Harness, &r.Runtime, &r.WorkingDir, &r.ConversationKey, &r.ConversationID,
		&r.Title, &r.Summary, &r.Turn, &r.Status, &by, &via, &answeredAt, &expiresAt, &r.DeliveryState,
		&claimedAt, &r.DeliveryPath, &deliveredRun, &deliveredTurn, &r.DeliveryError, &r.Source, &r.HopID, &r.TraceID,
		&createdAt, &updatedAt, &r.Revision); err != nil {
		return Request{}, err
	}
	r.DeliveredRunID = deliveredRun.String
	r.DeliveredTurn = int(deliveredTurn.Int64)
	var err error
	if r.Status != StatusPending && by.Valid {
		at, err := parseOptTime(answeredAt)
		if err != nil {
			return Request{}, err
		}
		r.Answered = &Answered{By: by.String, Via: via.String}
		if at != nil {
			r.Answered.At = *at
		}
	}
	if r.ExpiresAt, err = parseOptTime(expiresAt); err != nil {
		return Request{}, err
	}
	if r.ClaimedAt, err = parseOptTime(claimedAt); err != nil {
		return Request{}, err
	}
	if r.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Request{}, err
	}
	if r.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Request{}, err
	}
	return r, nil
}

func scanForm(rs rowScanner) (string, Form, error) {
	var id, payload, allow string
	var answer sql.NullString
	var f Form
	if err := rs.Scan(&id, &f.Idx, &f.Type, &f.Title, &f.Summary, &f.Target, &payload, &allow,
		&f.ReportKind, &f.ReportSHA256, &answer); err != nil {
		return "", Form{}, err
	}
	if err := json.Unmarshal([]byte(payload), &f.Payload); err != nil {
		return "", Form{}, fmt.Errorf("request %s form %d payload: %w", id, f.Idx, err)
	}
	if err := json.Unmarshal([]byte(allow), &f.Allow); err != nil {
		return "", Form{}, fmt.Errorf("request %s form %d allow: %w", id, f.Idx, err)
	}
	if answer.Valid && answer.String != "" {
		f.Answer = &FormAnswer{}
		if err := json.Unmarshal([]byte(answer.String), f.Answer); err != nil {
			return "", Form{}, fmt.Errorf("request %s form %d answer: %w", id, f.Idx, err)
		}
	}
	return id, f, nil
}

func parseOptTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
