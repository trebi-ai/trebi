package requests

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const requestColumns = `id, run_id, workspace_id, job_id,
  COALESCE((SELECT ji.name FROM job_index ji WHERE ji.id = requests.job_id), ''), kind, session_id, harness, runtime,
  working_dir, conversation_key, conversation_id, type, title, summary, target,
  payload_json, allow_json, report_kind, report_sha256, delivery, status,
  answer_json, expires_at, delivery_state, claimed_at, delivery_path,
  delivered_run_id, delivery_error, created_at, updated_at, revision`

type rowScanner interface{ Scan(dest ...any) error }

func scanRequest(rs rowScanner) (Request, error) {
	var r Request
	var payload, allow, createdAt, updatedAt string
	var answer, expiresAt, claimedAt, deliveredRun sql.NullString
	if err := rs.Scan(&r.ID, &r.RunID, &r.WorkspaceID, &r.JobID, &r.JobName, &r.Kind, &r.SessionID,
		&r.Harness, &r.Runtime, &r.WorkingDir, &r.ConversationKey, &r.ConversationID,
		&r.Type, &r.Title, &r.Summary, &r.Target, &payload, &allow, &r.ReportKind,
		&r.ReportSHA256, &r.Delivery, &r.Status, &answer, &expiresAt, &r.DeliveryState,
		&claimedAt, &r.DeliveryPath, &deliveredRun, &r.DeliveryError, &createdAt,
		&updatedAt, &r.Revision); err != nil {
		return Request{}, err
	}
	if err := json.Unmarshal([]byte(payload), &r.Payload); err != nil {
		return Request{}, fmt.Errorf("request %s payload: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(allow), &r.Allow); err != nil {
		return Request{}, fmt.Errorf("request %s allow: %w", r.ID, err)
	}
	if answer.Valid && answer.String != "" {
		r.Answer = &Answer{}
		if err := json.Unmarshal([]byte(answer.String), r.Answer); err != nil {
			return Request{}, fmt.Errorf("request %s answer: %w", r.ID, err)
		}
	}
	r.DeliveredRunID = deliveredRun.String
	var err error
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
