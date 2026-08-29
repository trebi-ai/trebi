package store

// SchemaVersion is the head migration version this binary expects.
const SchemaVersion = 4

// migrations returns the ordered schema versions (index i = version i+1). Append only.
// Full backend-0808 §8 schema in one migration for trebi greenfield.
//
// B10 "DB-backed objects" (monotonic revision, carried on the bus frame):
//
//	workspaces, approvals, runs, inbox_items
//
// Not in this change (internal bookkeeping, file index, or not yet a sync source):
//
//	integration_cursors, harness_state, skill_index, job_index,
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
	}
}
