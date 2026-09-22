package store

// SchemaVersion is the head migration version this binary expects.
const SchemaVersion = 19

// migrations returns the ordered schema versions (index i = version i+1). Append only.
// Full backend-0808 §8 schema in one migration for trebi greenfield.
//
// B10 "DB-backed objects" (monotonic revision, carried on the bus frame):
//
//	workspaces, approvals, runs, inbox_items
//
// Not in this change (internal bookkeeping, file index, or not yet a sync source):
//
//	connector_cursors, harness_state, skill_index, job_index,
//	event_queue, schedules, conversations
//
// AUTOINCREMENT (run_segments.id, run_events.id, event_log.seq) is a known
// Postgres-port blocker. Leave it until the port is scheduled; a migration
// purely for portability has no payoff on SQLite.
func migrations() []string { //nolint:funlen // static data table
	return []string{
		// v1: complete Phase-2 schema
		`
CREATE TABLE workspaces (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	path        TEXT NOT NULL,
	git_enabled INTEGER NOT NULL DEFAULT 0,
	orphaned    INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_ws_path ON workspaces(path);

CREATE TABLE job_index (
	workspace_id  TEXT NOT NULL,
	name          TEXT NOT NULL,
	path          TEXT NOT NULL,
	kind          TEXT NOT NULL DEFAULT 'job',
	active        INTEGER NOT NULL DEFAULT 1,
	harness       TEXT,
	hash          TEXT,
	parse_error   TEXT,
	updated_at    TEXT NOT NULL,
	PRIMARY KEY (workspace_id, name)
);

CREATE TABLE runs (
	id                       TEXT PRIMARY KEY,
	workspace_id             TEXT NOT NULL,
	job                      TEXT NOT NULL,
	kind                     TEXT NOT NULL DEFAULT 'job',
	reason                   TEXT,
	status                   TEXT NOT NULL,
	end_reason               TEXT,
	prompt_preview           TEXT,
	inputs_json              TEXT,
	trigger_event_id         TEXT,
	trigger_integration      TEXT,
	trigger_capture_json     TEXT,
	trigger_vars_json        TEXT,
	conversation_key         TEXT,
	conversation_id          TEXT,
	session_id               TEXT,
	runtime                  TEXT NOT NULL DEFAULT 'tui',
	harness                  TEXT,
	tmux_session             TEXT,
	tokens_input             INTEGER,
	tokens_output            INTEGER,
	tokens_cache_read        INTEGER,
	tokens_cache_creation    INTEGER,
	tokens_model             TEXT,
	started_at               TEXT NOT NULL,
	ended_at                 TEXT,
	max_duration_ms          INTEGER NOT NULL DEFAULT 0,
	idle_timeout_ms          INTEGER NOT NULL DEFAULT 0,
	keep_alive_ms            INTEGER NOT NULL DEFAULT 0,
	nudge_ms                 INTEGER NOT NULL DEFAULT -1,
	resumable_ms             INTEGER NOT NULL DEFAULT 0,
	resumed_from             TEXT,
	resume_attempts          INTEGER NOT NULL DEFAULT 0,
	queue_id                 TEXT,
	claimed_by               TEXT,
	trace_id                 TEXT NOT NULL,
	revision                 INTEGER NOT NULL DEFAULT 1,
	updated_at               TEXT NOT NULL
);
CREATE INDEX idx_runs_ws_started ON runs(workspace_id, started_at DESC);
CREATE INDEX idx_runs_job_started ON runs(job, started_at DESC);
CREATE INDEX idx_runs_status ON runs(status);
CREATE INDEX idx_runs_conversation ON runs(conversation_key);
CREATE INDEX idx_runs_ws_kind ON runs(workspace_id, kind, started_at DESC);
CREATE INDEX idx_runs_session ON runs(session_id);
CREATE UNIQUE INDEX idx_runs_queue_id ON runs(queue_id) WHERE queue_id IS NOT NULL AND queue_id != '';

CREATE TABLE run_segments (
	id            INTEGER PRIMARY KEY AUTOINCREMENT, -- Postgres-port: GENERATED ALWAYS AS IDENTITY
	run_id        TEXT    NOT NULL,
	layer         TEXT    NOT NULL,
	state         TEXT    NOT NULL,
	started_at    TEXT    NOT NULL,
	ended_at      TEXT,
	duration_ms   INTEGER,
	reason        TEXT,
	source        TEXT,
	detail_json   TEXT
);
CREATE INDEX idx_seg_run_layer ON run_segments(run_id, layer, started_at);
CREATE INDEX idx_seg_open ON run_segments(run_id, layer) WHERE ended_at IS NULL;

CREATE TABLE run_events (
	id            INTEGER PRIMARY KEY AUTOINCREMENT, -- Postgres-port: GENERATED ALWAYS AS IDENTITY
	run_id        TEXT    NOT NULL,
	at            TEXT    NOT NULL,
	kind          TEXT    NOT NULL,
	source        TEXT,
	detail_json   TEXT,
	trace_id      TEXT
);
CREATE INDEX idx_re_run_at ON run_events(run_id, at);

CREATE TABLE events (
	id              TEXT PRIMARY KEY,
	integration     TEXT NOT NULL,
	received_at     TEXT NOT NULL,
	summary         TEXT,
	raw_json        TEXT,
	trace_id        TEXT NOT NULL,
	cursor_id       TEXT
);
CREATE INDEX idx_events_integration ON events(integration, received_at DESC);

CREATE TABLE event_outcomes (
	event_id        TEXT NOT NULL,
	workspace_id    TEXT NOT NULL,
	job             TEXT NOT NULL,
	on_index        INTEGER NOT NULL DEFAULT 0,
	outcome         TEXT NOT NULL,
	queue_id        TEXT,
	error_message   TEXT,
	created_at      TEXT NOT NULL,
	PRIMARY KEY (event_id, workspace_id, job, on_index)
);

CREATE TABLE event_queue (
	id                 TEXT PRIMARY KEY,
	integration        TEXT NOT NULL,
	workspace_id       TEXT NOT NULL,
	job                TEXT NOT NULL,
	kind               TEXT NOT NULL DEFAULT 'job',
	on_index           INTEGER NOT NULL DEFAULT 0,
	conversation_key   TEXT NOT NULL DEFAULT '',
	event_id           TEXT,
	event_raw          TEXT,
	capture_json       TEXT,
	vars_json          TEXT,
	prompt             TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL,
	block_reason       TEXT,
	result             TEXT,
	attempts           INTEGER NOT NULL DEFAULT 0,
	next_attempt_at    TEXT,
	claimed_at         TEXT,
	run_id             TEXT,
	resume_of          TEXT,
	priority           INTEGER NOT NULL DEFAULT 0,
	error_message      TEXT,
	trace_id           TEXT,
	created_at         TEXT NOT NULL,
	updated_at         TEXT NOT NULL,
	done_at            TEXT
);
CREATE INDEX idx_eq_status_next ON event_queue(status, next_attempt_at, created_at);
CREATE INDEX idx_eq_lane ON event_queue(workspace_id, job, kind, conversation_key, status, created_at);
CREATE INDEX idx_eq_integration ON event_queue(integration, created_at DESC);

CREATE TABLE schedules (
	workspace_id   TEXT NOT NULL,
	job            TEXT NOT NULL,
	idx            INTEGER NOT NULL DEFAULT 0,
	spec           TEXT NOT NULL,
	timezone       TEXT NOT NULL DEFAULT 'UTC',
	catch_up       INTEGER NOT NULL DEFAULT 1,
	next_due_at    TEXT,
	last_fired_at  TEXT,
	anchor         TEXT,
	updated_at     TEXT NOT NULL,
	PRIMARY KEY (workspace_id, job, idx)
);
CREATE INDEX idx_sched_due ON schedules(next_due_at);

CREATE TABLE dispatch_policy (
	key            TEXT PRIMARY KEY,
	last_fired_at  TEXT,
	window_json    TEXT
);

CREATE TABLE integration_cursors (
	name            TEXT PRIMARY KEY,
	last_event_id   TEXT NOT NULL DEFAULT '',
	updated_at      TEXT NOT NULL
);

CREATE TABLE integration_metrics (
	name            TEXT PRIMARY KEY,
	events_total    INTEGER NOT NULL DEFAULT 0,
	parse_errors    INTEGER NOT NULL DEFAULT 0,
	match_errors    INTEGER NOT NULL DEFAULT 0,
	fires           INTEGER NOT NULL DEFAULT 0,
	drops           INTEGER NOT NULL DEFAULT 0,
	events_24h      INTEGER NOT NULL DEFAULT 0,
	fires_24h       INTEGER NOT NULL DEFAULT 0,
	last_event_at   TEXT,
	updated_at      TEXT NOT NULL
);

CREATE TABLE conversations (
	id              TEXT PRIMARY KEY,
	workspace_id    TEXT NOT NULL,
	key             TEXT NOT NULL,
	title           TEXT,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_conv_ws_key ON conversations(workspace_id, key);

CREATE TABLE conversation_channels (
	id              TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	channel_type    TEXT NOT NULL,
	channel_ref     TEXT NOT NULL,
	created_at      TEXT NOT NULL
);

CREATE TABLE channel_pointers (
	channel_type    TEXT NOT NULL,
	channel_ref     TEXT NOT NULL,
	conversation_id TEXT NOT NULL,
	updated_at      TEXT NOT NULL,
	PRIMARY KEY (channel_type, channel_ref)
);

CREATE TABLE inbox_items (
	id              TEXT PRIMARY KEY,
	kind            TEXT NOT NULL,
	status          TEXT NOT NULL DEFAULT 'unread',
	subject         TEXT,
	title           TEXT,
	body            TEXT,
	hint            TEXT,
	run_id          TEXT,
	workspace_id    TEXT,
	job             TEXT,
	alert_kind      TEXT,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL,
	resolved_at     TEXT
);
CREATE INDEX idx_inbox_status ON inbox_items(status, created_at DESC);

CREATE TABLE approvals (
	id              TEXT PRIMARY KEY,
	run_id          TEXT NOT NULL,
	question        TEXT,
	status          TEXT NOT NULL DEFAULT 'pending',
	decision        TEXT,
	answered_in_session INTEGER NOT NULL DEFAULT 0,
	created_at      TEXT NOT NULL,
	decided_at      TEXT
);
CREATE INDEX idx_approvals_run ON approvals(run_id);

CREATE TABLE skill_index (
	name            TEXT PRIMARY KEY,
	provenance      TEXT NOT NULL,
	path            TEXT NOT NULL,
	hash            TEXT,
	used_30d        INTEGER NOT NULL DEFAULT 0,
	updated_at      TEXT NOT NULL
);

CREATE TABLE harness_state (
	name            TEXT PRIMARY KEY,
	auth_ok         INTEGER,
	limit_exceeded  INTEGER,
	limit_resets_at TEXT,
	usage_percent   REAL,
	updated_at      TEXT NOT NULL
);

CREATE TABLE cloud_cursors (
	stream          TEXT PRIMARY KEY,
	last_seq        INTEGER NOT NULL DEFAULT 0,
	updated_at      TEXT NOT NULL
);

CREATE TABLE event_log (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT, -- Postgres-port: GENERATED ALWAYS AS IDENTITY
	event_id    TEXT NOT NULL,
	topic       TEXT NOT NULL,
	ts          TEXT NOT NULL,
	trace_id    TEXT,
	entity      TEXT,
	entity_id   TEXT,
	action      TEXT,
	revision    INTEGER,
	attrs_json  TEXT
);
CREATE INDEX idx_bus_topic ON event_log(topic, seq);

CREATE TABLE system_flags (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
		// v2: B10 revision on bus-entity tables that cloudlink will sync.
		// Append-only — v1 may already be applied on developer DBs.
		`
ALTER TABLE workspaces ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE approvals ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
`,
		// v3: inbox_items columns the alerts service selects (inboxCols).
		// These were briefly written into the v1 body; DBs created before that
		// edit never got them, so they are re-applied here as their own step.
		`
ALTER TABLE inbox_items ADD COLUMN integration TEXT;
ALTER TABLE inbox_items ADD COLUMN actions TEXT;
ALTER TABLE inbox_items ADD COLUMN trace_id TEXT;
ALTER TABLE inbox_items ADD COLUMN escalates_at TEXT;
ALTER TABLE inbox_items ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_inbox_kind_subject ON inbox_items(alert_kind, subject, status);
`,
		// v4: conversation wire fields + durable message rows (assistant chat).
		`
ALTER TABLE conversations ADD COLUMN origin_channel   TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN summary          TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN last_activity_at TEXT;
ALTER TABLE conversations ADD COLUMN revision         INTEGER NOT NULL DEFAULT 1;

CREATE TABLE conversation_messages (
	id              TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	role            TEXT NOT NULL,
	channel_type    TEXT NOT NULL DEFAULT '',
	channel_ref     TEXT NOT NULL DEFAULT '',
	text            TEXT NOT NULL,
	run_id          TEXT,
	turn_index      INTEGER,
	idempotency_key TEXT,
	created_at      TEXT NOT NULL
);
CREATE INDEX idx_convmsg_conv ON conversation_messages(conversation_id, id);
CREATE UNIQUE INDEX idx_convmsg_turn ON conversation_messages(run_id, turn_index)
	WHERE run_id IS NOT NULL AND turn_index IS NOT NULL;
CREATE UNIQUE INDEX idx_convmsg_idem ON conversation_messages(conversation_id, idempotency_key)
	WHERE idempotency_key IS NOT NULL;
`,
		// v5: harness auth probe columns (status, detail, probed_at, expires_at).
		`
ALTER TABLE harness_state ADD COLUMN auth_status TEXT;
ALTER TABLE harness_state ADD COLUMN auth_detail TEXT;
ALTER TABLE harness_state ADD COLUMN auth_probed_at TEXT;
ALTER TABLE harness_state ADD COLUMN auth_expires_at TEXT;
`,
		// v6: idempotency keys for repeat-safe POSTs (§3.4).
		`
CREATE TABLE idempotency_keys (
	scope           TEXT NOT NULL,
	key             TEXT NOT NULL,
	result_id       TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	PRIMARY KEY (scope, key)
);
CREATE INDEX idx_idempotency_created ON idempotency_keys(created_at);
`,
		// v7: skill source, so refresh can re-pull (A6.10).
		`
ALTER TABLE skill_index ADD COLUMN source_kind TEXT;
ALTER TABLE skill_index ADD COLUMN source_ref TEXT;
`,
		// v8: the directory a run launched in. Codex mints its own rollout id,
		// so its transcript is found by path; without this column every reader
		// had to re-parse the JOB.md to guess where the run started.
		`
ALTER TABLE runs ADD COLUMN working_dir TEXT;
`,
		// v9: integration → connector (object-family rename).
		`
DROP INDEX IF EXISTS idx_events_integration;
ALTER TABLE events RENAME COLUMN integration TO connector;
CREATE INDEX idx_events_connector ON events(connector, received_at DESC);

DROP INDEX IF EXISTS idx_eq_integration;
ALTER TABLE event_queue RENAME COLUMN integration TO connector;
CREATE INDEX idx_eq_connector ON event_queue(connector, created_at DESC);

ALTER TABLE runs RENAME COLUMN trigger_integration TO trigger_connector;
ALTER TABLE inbox_items RENAME COLUMN integration TO connector;
ALTER TABLE integration_cursors RENAME TO connector_cursors;
ALTER TABLE integration_metrics RENAME TO connector_metrics;

UPDATE inbox_items SET alert_kind='connector_unhealthy' WHERE alert_kind='integration_unhealthy';
UPDATE skill_index SET provenance='connector' WHERE provenance='integration';
`,
		// v10: native assistant UTC-day token counter.
		`
CREATE TABLE assistant_usage (
	day    TEXT PRIMARY KEY,
	tokens INTEGER NOT NULL DEFAULT 0
);
`,
		// v11: run_segments layer rename (agent→session, task→run) so the
		// "agent" noun is free for the product object.
		`
UPDATE run_segments SET layer='session' WHERE layer='agent';
UPDATE run_segments SET layer='run' WHERE layer='task';
`,
		// v12: agent ownership — job_index.owner is the owning agent's index
		// name (agents/<name>); '' = unowned.
		`
ALTER TABLE job_index ADD COLUMN owner TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_job_index_owner ON job_index(workspace_id, owner);
`,
		// v13: launch-time MEMORY.md hash. The reaper freshness check compares
		// it to the file at run close; '' = memory off for that run.
		`
ALTER TABLE runs ADD COLUMN memory_hash TEXT NOT NULL DEFAULT '';
`,
		// v14: human outcome over a terminal run (complete|failed|skipped).
		// status/end_reason stay the machine record; the outcome drives
		// display, job health, and the inbox.
		`
ALTER TABLE runs ADD COLUMN outcome TEXT;
ALTER TABLE runs ADD COLUMN outcome_note TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN outcome_at TEXT;
`,
		// v15: subprocess runtime pid + attach kind for the CLI run runtime
		// (2026-09-16 harness-subprocess-runtime). runtime_pid is the child
		// process for boot reconcile; attach_kind is events for CLI runs.
		`
ALTER TABLE runs ADD COLUMN runtime_pid INTEGER;
ALTER TABLE runs ADD COLUMN attach_kind TEXT;
`,
		// v16: keyed approvals. Wire permissions carry a vendor request id so
		// concurrent requests on one run each get an answerable row (A.10);
		// pane/hook approvals keep the empty key and one-pending-per-run rule.
		`
ALTER TABLE approvals ADD COLUMN permission_key TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_approvals_key ON approvals(run_id, permission_key);
`,
		// v17: email channel (plan 2026-09-19 Part G). email_addresses is the
		// source of truth for every workspace address; email_messages is the
		// envelope log, never bodies. inbox_items.email_message_id closes the
		// alert loop: a reply resolves to the item by Message-ID.
		`
CREATE TABLE email_addresses (
  workspace_id  TEXT PRIMARY KEY,
  local_part    TEXT NOT NULL UNIQUE,
  domain        TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  rotated_at    TEXT,
  revision      INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE email_messages (
  id               TEXT PRIMARY KEY,
  direction        TEXT NOT NULL,
  workspace_id     TEXT NOT NULL DEFAULT '',
  agent            TEXT NOT NULL DEFAULT '',
  message_id       TEXT NOT NULL DEFAULT '',
  in_reply_to      TEXT NOT NULL DEFAULT '',
  thread_root      TEXT NOT NULL DEFAULT '',
  sender           TEXT NOT NULL DEFAULT '',
  recipients       TEXT NOT NULL DEFAULT '',
  subject          TEXT NOT NULL DEFAULT '',
  dmarc            TEXT NOT NULL DEFAULT '',
  disposition      TEXT NOT NULL,
  inbox_id         TEXT,
  run_id           TEXT,
  conversation_id  TEXT,
  attachments      TEXT NOT NULL DEFAULT '[]',
  trace_id         TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  revision         INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX email_messages_message_id ON email_messages(message_id);
CREATE INDEX email_messages_thread ON email_messages(thread_root, created_at);
ALTER TABLE inbox_items ADD COLUMN email_message_id TEXT NOT NULL DEFAULT '';
`,
		// v18: reply-by-mail marker (plan 2026-09-19 Part I.2). covered_turn is
		// the turn index one outbound reply already covered; the tick compares
		// it to new turns, so a restart cannot resend or skip.
		`
ALTER TABLE email_messages ADD COLUMN covered_turn INTEGER;
`,
		// v19: cloud link (plan 2026-09-22 Part A). cloud_link holds the one
		// device identity row; a missing row means unlinked and the private
		// key never lands here (it lives in the secrets directory).
		// cloud_addresses and cloud_hooks are the local registry rows for the
		// hosted objects the worker mints. cloud_hooks.url holds the full
		// public URL including the token: treat it as a weak credential.
		`
CREATE TABLE cloud_link (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  device_id       TEXT NOT NULL,
  device_name     TEXT NOT NULL,
  api_url         TEXT NOT NULL,
  account_email   TEXT NOT NULL DEFAULT '',
  plan            TEXT NOT NULL DEFAULT '',
  features        TEXT NOT NULL DEFAULT '[]',
  linked_at       TEXT NOT NULL,
  refreshed_at    TEXT NOT NULL
);
CREATE TABLE cloud_addresses (
  id              TEXT PRIMARY KEY,
  workspace_id    TEXT NOT NULL,
  address         TEXT NOT NULL,
  local_part      TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  UNIQUE (workspace_id)
);
CREATE TABLE cloud_hooks (
  id              TEXT PRIMARY KEY,
  workspace_id    TEXT NOT NULL,
  label           TEXT NOT NULL,
  url             TEXT NOT NULL,
  created_at      TEXT NOT NULL
);
`,
	}
}
