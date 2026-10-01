package store

// SchemaVersion is the head migration version this binary expects.
const SchemaVersion = 36

// migrations returns the ordered schema versions (index i = version i+1). Append only.
// Full backend-0808 §8 schema in one migration for trebi greenfield.
//
// B10 "DB-backed objects" (monotonic revision, carried on the bus frame):
//
//	workspaces, approvals, runs, inbox_items
//
// Not in this change (internal bookkeeping, file index, or not yet a sync source):
//
//	harness_state, skill_index, job_index,
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
		// v20: conversation operator (2026-09-24 plan D.3). Empty is the
		// workspace assistant; agents/<id> is one agent operator.
		`
ALTER TABLE conversations ADD COLUMN operator TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_conv_operator ON conversations(workspace_id, operator, last_activity_at);
`,
		// v21: run actor token (2026-09-24 plan E.1). Only the SHA-256 hash is
		// stored; the plaintext lives in the run process environment.
		`
ALTER TABLE runs ADD COLUMN actor_token_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_runs_actor_token ON runs(actor_token_hash) WHERE actor_token_hash != '';
`,
		// v22: human-approved AGENT.md changes. A proposal captures both the
		// proposed bytes and the bytes it was reviewed against; approval uses
		// If-Match so a silently changed file can never be overwritten. The
		// email purpose supports restart-safe report threading/dedupe.
		`
CREATE TABLE agent_proposals (
  id             TEXT PRIMARY KEY,
  workspace_id   TEXT NOT NULL,
  agent          TEXT NOT NULL,
  run_id         TEXT NOT NULL DEFAULT '',
  rel_path       TEXT NOT NULL,
  base_hash      TEXT NOT NULL,
  base_content   TEXT NOT NULL,
  proposed       TEXT NOT NULL,
  note           TEXT NOT NULL DEFAULT '',
  protected_keys TEXT NOT NULL DEFAULT '[]',
  status         TEXT NOT NULL DEFAULT 'pending',
  decided_by     TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  decided_at     TEXT,
  revision       INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_agent_proposals_pending ON agent_proposals(workspace_id, agent, status);
ALTER TABLE email_messages ADD COLUMN purpose TEXT NOT NULL DEFAULT '';
`,
		// v23: one-time assistant seed flag. Marks workspaces already offered
		// the default TREBI.md, so a deliberately deleted assistant never
		// comes back on daemon start (2026-09-24 plan K.3).
		`
ALTER TABLE workspaces ADD COLUMN assistant_seeded INTEGER NOT NULL DEFAULT 0;
`,
		// v24: assistant sessions (plan 2026-09-26 Parts B-C). The assistant
		// is no longer a job or a run. assistant_sessions holds the runtime
		// state of one assistant conversation; conversation_events is its
		// replayable transcript. The clean cutoff deletes the kind=assistant
		// job rows, queue items, and runs, and drops the seed flag and the
		// native usage table it replaces.
		`
CREATE TABLE assistant_sessions (
  conversation_id  TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
  workspace_id     TEXT NOT NULL,
  harness          TEXT NOT NULL,
  model            TEXT NOT NULL DEFAULT '',
  effort           TEXT NOT NULL DEFAULT '',
  mode             TEXT NOT NULL,
  vendor_id        TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL,
  error            TEXT NOT NULL DEFAULT '',
  turn             INTEGER NOT NULL DEFAULT 0,
  prompts          INTEGER NOT NULL DEFAULT 0,
  tokens_in        INTEGER NOT NULL DEFAULT 0,
  tokens_out       INTEGER NOT NULL DEFAULT 0,
  tokens_cached    INTEGER NOT NULL DEFAULT 0,
  cost_usd         REAL NOT NULL DEFAULT 0,
  actor_token_hash TEXT NOT NULL DEFAULT '',
  last_message     TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
CREATE INDEX idx_assistant_sessions_ws ON assistant_sessions(workspace_id, updated_at);
CREATE INDEX idx_assistant_sessions_token ON assistant_sessions(actor_token_hash);
CREATE TABLE conversation_events (
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  seq             INTEGER NOT NULL,
  turn            INTEGER NOT NULL DEFAULT 0,
  type            TEXT NOT NULL,
  body            TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (conversation_id, seq)
);
DELETE FROM job_index WHERE kind = 'assistant';
DELETE FROM event_queue WHERE kind = 'assistant' AND status IN ('pending','claimed');
DELETE FROM runs WHERE kind = 'assistant';
DROP TABLE assistant_usage;
ALTER TABLE workspaces DROP COLUMN assistant_seeded;
`,
		// v25: title untitled conversations from their first user message
		// (plan 2026-09-26 assistant-settings-annotation-fixes E). The first
		// line, 60 characters, then an ellipsis, like deriveTitle in Go.
		// SQLite length and substr count characters, which matches the rune
		// count of deriveTitle.
		`
UPDATE conversations
SET title = (
  SELECT CASE
    WHEN length(t) > 60 THEN substr(t, 1, 60) || '…'
    ELSE t END
  FROM (
    SELECT trim(CASE WHEN instr(text, char(10)) > 0
                     THEN substr(text, 1, instr(text, char(10)) - 1)
                     ELSE text END) AS t
    FROM conversation_messages m
    WHERE m.conversation_id = conversations.id AND m.role = 'user'
    ORDER BY m.id LIMIT 1
  )
)
WHERE COALESCE(title, '') = ''
  AND EXISTS (SELECT 1 FROM conversation_messages m
              WHERE m.conversation_id = conversations.id AND m.role = 'user');
`,
		// v26: connectors as one object (plan 2026-09-27 D). Events gain the
		// dedup key, the declared identity, the channel ref, and the catch-up
		// flag. cursor_id was never written, so the rename loses no data. The
		// metrics and policy tables have no reader or writer: counters live on
		// the connector process and policy lives in memory.
		`
ALTER TABLE events RENAME COLUMN cursor_id TO source_id;
ALTER TABLE events ADD COLUMN identity TEXT;
ALTER TABLE events ADD COLUMN channel_ref TEXT;
ALTER TABLE events ADD COLUMN catch_up INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_events_source ON events(connector, source_id) WHERE source_id IS NOT NULL;
DROP TABLE connector_metrics;
DROP TABLE dispatch_policy;
`,
		// v27: channels as a workspace-scoped pointer and one outbox
		// (plan 2026-09-27 G.1). Connectors are global, so two workspaces
		// that follow the same chat need two conversations: workspace_id
		// joins the pointer key. channel_outbox holds one reply per
		// assistant message, keyed by that message id.
		`
CREATE TABLE channel_pointers_new (
    workspace_id    TEXT NOT NULL,
    channel_type    TEXT NOT NULL,
    channel_ref     TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (workspace_id, channel_type, channel_ref)
);
INSERT INTO channel_pointers_new
    SELECT c.workspace_id, p.channel_type, p.channel_ref, p.conversation_id, p.updated_at
    FROM channel_pointers p JOIN conversations c ON c.id = p.conversation_id;
DROP TABLE channel_pointers;
ALTER TABLE channel_pointers_new RENAME TO channel_pointers;

CREATE TABLE channel_outbox (
    id               TEXT PRIMARY KEY,
    connector        TEXT NOT NULL,
    workspace_id     TEXT NOT NULL,
    conversation_id  TEXT NOT NULL,
    channel_ref      TEXT NOT NULL,
    message_id       TEXT NOT NULL,
    run_id           TEXT,
    text             TEXT NOT NULL,
    status           TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TEXT,
    error_message    TEXT,
    created_at       TEXT NOT NULL,
    sent_at          TEXT
);
CREATE UNIQUE INDEX idx_channel_outbox_message ON channel_outbox(message_id);
CREATE INDEX idx_channel_outbox_due ON channel_outbox(status, next_attempt_at);
`,
		// v28: a run keeps the connector names its launch resolved, so a
		// resume gives the agent the same actions (plan 2026-09-27 review
		// fixes P1-20).
		`
ALTER TABLE runs ADD COLUMN connectors_json TEXT;
`,
		// v29: one outbox for every outgoing send (plan 2026-09-28 A.1).
		// It replaces channel_outbox. The body stays on the row until the
		// row is sent. email_messages keeps the envelope and points at its
		// outbox row. inbox_items records the alert delivery state (D.2).
		`
CREATE TABLE outbox (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    purpose         TEXT NOT NULL,
    workspace_id    TEXT NOT NULL DEFAULT '',
    connector       TEXT NOT NULL DEFAULT '',
    target          TEXT NOT NULL DEFAULT '',
    subject         TEXT NOT NULL DEFAULT '',
    body            TEXT,
    env_json        TEXT NOT NULL DEFAULT '{}',
    conversation_id TEXT,
    run_id          TEXT,
    inbox_id        TEXT,
    email_row_id    TEXT,
    dedupe_key      TEXT,
    route_step      INTEGER,
    route_json      TEXT,
    status          TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 5,
    next_attempt_at TEXT,
    claimed_at      TEXT,
    error_message   TEXT,
    created_at      TEXT NOT NULL,
    sent_at         TEXT,
    revision        INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_outbox_dedupe ON outbox(dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX idx_outbox_due ON outbox(status, next_attempt_at);
CREATE INDEX idx_outbox_ws ON outbox(workspace_id, created_at DESC);
CREATE INDEX idx_outbox_inbox ON outbox(inbox_id) WHERE inbox_id IS NOT NULL;
CREATE INDEX idx_outbox_conv ON outbox(conversation_id) WHERE conversation_id IS NOT NULL;

INSERT INTO outbox (id, kind, purpose, workspace_id, connector, target, body, conversation_id,
    run_id, dedupe_key, status, attempts, next_attempt_at, error_message, created_at, sent_at)
SELECT id, 'channel', 'reply', workspace_id, connector, channel_ref,
    CASE WHEN status='sent' THEN NULL ELSE text END, conversation_id, run_id, message_id,
    status, attempts, next_attempt_at, error_message, created_at, sent_at
FROM channel_outbox;
DROP TABLE channel_outbox;

ALTER TABLE email_messages ADD COLUMN outbox_id TEXT;
UPDATE email_messages SET disposition='send_failed' WHERE disposition='sending';
ALTER TABLE inbox_items ADD COLUMN delivery TEXT;
`,
		// v30: connector instances, rooms, and threads (plan 2026-09-28
		// connector manifest E.5). channel_type and channel_ref become
		// connector and room, plus instance and thread. The instance of an
		// old row is its connector. An email room is the mailbox key (the
		// workspace id), and the email thread is the old channel ref (the
		// thread root). The outbox keeps email recipients in their own
		// column. Connector cursors key by instance.
		`
CREATE TABLE events_new (
    id          TEXT PRIMARY KEY,
    connector   TEXT NOT NULL,
    instance    TEXT NOT NULL DEFAULT '',
    received_at TEXT NOT NULL,
    summary     TEXT,
    raw_json    TEXT,
    trace_id    TEXT NOT NULL,
    source_id   TEXT,
    identity    TEXT,
    event_type  TEXT,
    room        TEXT,
    thread      TEXT,
    sender      TEXT,
    catch_up    INTEGER NOT NULL DEFAULT 0
);
INSERT INTO events_new (id, connector, instance, received_at, summary, raw_json, trace_id,
    source_id, identity, room, catch_up)
SELECT id, connector, connector, received_at, summary, raw_json, trace_id,
    source_id, identity, channel_ref, catch_up FROM events;
DROP TABLE events;
ALTER TABLE events_new RENAME TO events;
CREATE INDEX idx_events_connector ON events(connector, received_at DESC);
CREATE UNIQUE INDEX idx_events_source ON events(instance, source_id) WHERE source_id IS NOT NULL;
CREATE INDEX idx_events_room ON events(instance, room);

CREATE TABLE conversation_channels_new (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    connector       TEXT NOT NULL,
    instance        TEXT NOT NULL,
    room            TEXT NOT NULL,
    thread          TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);
INSERT INTO conversation_channels_new
SELECT ch.id, ch.conversation_id, ch.channel_type, ch.channel_type,
    CASE WHEN ch.channel_type='email' THEN c.workspace_id ELSE ch.channel_ref END,
    CASE WHEN ch.channel_type='email' THEN ch.channel_ref ELSE '' END,
    ch.created_at
FROM conversation_channels ch JOIN conversations c ON c.id = ch.conversation_id;
DROP TABLE conversation_channels;
ALTER TABLE conversation_channels_new RENAME TO conversation_channels;
CREATE INDEX idx_convch_conv ON conversation_channels(conversation_id);

CREATE TABLE channel_pointers_new (
    workspace_id    TEXT NOT NULL,
    connector       TEXT NOT NULL,
    instance        TEXT NOT NULL,
    room            TEXT NOT NULL,
    thread          TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (workspace_id, instance, room, thread)
);
INSERT OR IGNORE INTO channel_pointers_new
SELECT workspace_id, channel_type, channel_type,
    CASE WHEN channel_type='email' THEN workspace_id ELSE channel_ref END,
    CASE WHEN channel_type='email' THEN channel_ref ELSE '' END,
    conversation_id, updated_at
FROM channel_pointers;
DROP TABLE channel_pointers;
ALTER TABLE channel_pointers_new RENAME TO channel_pointers;

CREATE TABLE conversation_messages_new (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    role            TEXT NOT NULL,
    connector       TEXT NOT NULL DEFAULT '',
    instance        TEXT NOT NULL DEFAULT '',
    room            TEXT NOT NULL DEFAULT '',
    thread          TEXT NOT NULL DEFAULT '',
    text            TEXT NOT NULL,
    run_id          TEXT,
    turn_index      INTEGER,
    idempotency_key TEXT,
    created_at      TEXT NOT NULL
);
INSERT INTO conversation_messages_new
SELECT m.id, m.conversation_id, m.role, m.channel_type, m.channel_type,
    CASE WHEN m.channel_type='email' THEN c.workspace_id ELSE m.channel_ref END,
    CASE WHEN m.channel_type='email' THEN m.channel_ref ELSE '' END,
    m.text, m.run_id, m.turn_index, m.idempotency_key, m.created_at
FROM conversation_messages m JOIN conversations c ON c.id = m.conversation_id;
DROP TABLE conversation_messages;
ALTER TABLE conversation_messages_new RENAME TO conversation_messages;
CREATE INDEX idx_convmsg_conv ON conversation_messages(conversation_id, id);
CREATE UNIQUE INDEX idx_convmsg_turn ON conversation_messages(run_id, turn_index)
    WHERE run_id IS NOT NULL AND turn_index IS NOT NULL;
CREATE UNIQUE INDEX idx_convmsg_idem ON conversation_messages(conversation_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE outbox_new (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    purpose         TEXT NOT NULL,
    workspace_id    TEXT NOT NULL DEFAULT '',
    instance        TEXT NOT NULL DEFAULT '',
    room            TEXT NOT NULL DEFAULT '',
    thread          TEXT NOT NULL DEFAULT '',
    recipients      TEXT NOT NULL DEFAULT '',
    subject         TEXT NOT NULL DEFAULT '',
    body            TEXT,
    env_json        TEXT NOT NULL DEFAULT '{}',
    conversation_id TEXT,
    run_id          TEXT,
    inbox_id        TEXT,
    email_row_id    TEXT,
    dedupe_key      TEXT,
    route_step      INTEGER,
    route_json      TEXT,
    status          TEXT NOT NULL,
    hold_reason     TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 5,
    next_attempt_at TEXT,
    claimed_at      TEXT,
    error_message   TEXT,
    created_at      TEXT NOT NULL,
    sent_at         TEXT,
    revision        INTEGER NOT NULL DEFAULT 0
);
INSERT INTO outbox_new (id, kind, purpose, workspace_id, instance, room, recipients, subject, body,
    env_json, conversation_id, run_id, inbox_id, email_row_id, dedupe_key, route_step, route_json,
    status, attempts, max_attempts, next_attempt_at, claimed_at, error_message, created_at, sent_at, revision)
SELECT id, kind, purpose, workspace_id, connector,
    CASE WHEN kind='email' THEN '' ELSE target END,
    CASE WHEN kind='email' THEN target ELSE '' END,
    subject, body, env_json, conversation_id, run_id, inbox_id, email_row_id, dedupe_key, route_step,
    route_json, status, attempts, max_attempts, next_attempt_at, claimed_at, error_message,
    created_at, sent_at, revision
FROM outbox;
DROP TABLE outbox;
ALTER TABLE outbox_new RENAME TO outbox;
CREATE UNIQUE INDEX idx_outbox_dedupe ON outbox(dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX idx_outbox_due ON outbox(status, next_attempt_at);
CREATE INDEX idx_outbox_ws ON outbox(workspace_id, created_at DESC);
CREATE INDEX idx_outbox_inbox ON outbox(inbox_id) WHERE inbox_id IS NOT NULL;
CREATE INDEX idx_outbox_conv ON outbox(conversation_id) WHERE conversation_id IS NOT NULL;
CREATE INDEX idx_outbox_hold ON outbox(instance, hold_reason) WHERE hold_reason <> '';

ALTER TABLE connector_cursors RENAME COLUMN name TO instance;

CREATE TABLE channel_threads (
    instance          TEXT NOT NULL,
    room              TEXT NOT NULL,
    thread            TEXT NOT NULL,
    title             TEXT NOT NULL DEFAULT '',
    participants_json TEXT NOT NULL DEFAULT '[]',
    last_activity_at  TEXT,
    PRIMARY KEY (instance, room, thread)
);

CREATE TABLE connector_installs (
    name         TEXT NOT NULL,
    version      TEXT NOT NULL,
    digest       TEXT NOT NULL DEFAULT '',
    bin_path     TEXT NOT NULL DEFAULT '',
    installed_at TEXT NOT NULL,
    PRIMARY KEY (name, version)
);
`,
		// v31: requests (plan 2026-09-28). A request is a decision point that a
		// job run opens for a person; its run parks until the answer.
		`
CREATE TABLE requests (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL,
    workspace_id     TEXT NOT NULL,
    job              TEXT NOT NULL,
    kind             TEXT NOT NULL,
    session_id       TEXT NOT NULL DEFAULT '',
    harness          TEXT NOT NULL DEFAULT '',
    runtime          TEXT NOT NULL DEFAULT '',
    working_dir      TEXT NOT NULL DEFAULT '',
    conversation_key TEXT NOT NULL DEFAULT '',
    conversation_id  TEXT NOT NULL DEFAULT '',
    type             TEXT NOT NULL,
    title            TEXT NOT NULL,
    summary          TEXT NOT NULL DEFAULT '',
    target           TEXT NOT NULL DEFAULT '',
    payload_json     TEXT NOT NULL DEFAULT '{}',
    allow_json       TEXT NOT NULL,
    report_kind      TEXT NOT NULL DEFAULT '',
    report_sha256    TEXT NOT NULL DEFAULT '',
    delivery         TEXT NOT NULL DEFAULT 'resume',
    status           TEXT NOT NULL DEFAULT 'pending',
    answer_json      TEXT,
    answered_at      TEXT,
    expires_at       TEXT,
    delivery_state   TEXT NOT NULL DEFAULT 'none',
    claimed_at       TEXT,
    delivery_path    TEXT NOT NULL DEFAULT '',
    delivered_run_id TEXT,
    delivery_error   TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    revision         INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_requests_run ON requests(run_id);
CREATE INDEX idx_requests_open ON requests(status) WHERE status = 'pending';
CREATE INDEX idx_requests_undelivered ON requests(delivery_state)
    WHERE delivery = 'resume' AND status <> 'pending' AND delivery_state IN ('none','claimed');
ALTER TABLE inbox_items ADD COLUMN request_id TEXT;
`,
		// v32: workspace handle, the fixed name in the email address. The old
		// long random addresses go; the daemon mints <handle>-<token> again.
		`
ALTER TABLE workspaces ADD COLUMN handle TEXT NOT NULL DEFAULT '';
UPDATE workspaces SET handle = trim(substr(replace(replace(replace(replace(
    lower(trim(name)), ' ', '-'), '_', '-'), '.', '-'), '/', '-'), 1, 20), '-');
UPDATE workspaces SET handle = 'ws' WHERE length(handle) < 2 OR handle GLOB '*[^a-z0-9-]*';
DELETE FROM email_addresses;
DELETE FROM cloud_addresses;
`,
		// v33: connections (plan 2026-09-30). cursor.json in the connection
		// folder replaces connector_cursors. Old rows name connectors that no
		// connection has, so the rows of every non-native connector go.
		`
DROP TABLE connector_cursors;
DROP TABLE connector_installs;
CREATE TABLE connector_installs (
    type         TEXT NOT NULL,
    version      TEXT NOT NULL,
    digest       TEXT NOT NULL DEFAULT '',
    bin_path     TEXT NOT NULL DEFAULT '',
    installed_at TEXT NOT NULL,
    PRIMARY KEY (type, version)
);
DELETE FROM events WHERE connector NOT IN ('email','webhook');
DELETE FROM event_queue WHERE connector NOT IN ('email','webhook');
DELETE FROM outbox WHERE instance NOT IN ('email','webhook');
DELETE FROM conversation_channels WHERE instance NOT IN ('email','webhook');
DELETE FROM channel_pointers WHERE instance NOT IN ('email','webhook');
DELETE FROM conversation_messages WHERE instance IS NOT NULL AND instance <> '' AND instance NOT IN ('email','webhook');
DELETE FROM channel_threads WHERE instance NOT IN ('email','webhook');
DELETE FROM inbox_items WHERE connector IS NOT NULL AND connector <> '' AND connector NOT IN ('email','webhook');
UPDATE runs SET trigger_connector = NULL WHERE trigger_connector IS NOT NULL AND trigger_connector NOT IN ('email','webhook');
UPDATE runs SET connectors_json = NULL WHERE connectors_json IS NOT NULL AND connectors_json NOT IN ('', '[]', 'null');
`,
		// v34: trigger flood control (plan 2026-09-30). A manual or
		// scheduled launch that waits for a live run keeps its request.
		`ALTER TABLE event_queue ADD COLUMN launch_json TEXT;`,
		// v35: the recent senders of a connection read events by instance,
		// newest first (plan 2026-10-01 B.3).
		`CREATE INDEX idx_events_instance ON events(instance, received_at DESC);`,
		// v36: channel conversations (plan 2026-10-01 channel conversations
		// E.1). The unique source index serves the parent lookup. text keeps
		// the readable text of a native event for a redispatch.
		`
ALTER TABLE events ADD COLUMN text TEXT;
ALTER TABLE events ADD COLUMN reply_to TEXT;
ALTER TABLE events ADD COLUMN root TEXT;
CREATE INDEX idx_events_instance_root ON events(instance, root, received_at);
ALTER TABLE event_queue ADD COLUMN follow_up INTEGER NOT NULL DEFAULT 0;
`,
	}
}
