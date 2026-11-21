package store

import _ "embed"

// SchemaVersion is the head migration version this binary expects.
const SchemaVersion = 80

// baseVersion is the version of baseSchema. A store below it does not open.
const baseVersion = 80

// migrations returns the steps after the base schema. Index i is version baseVersion+1+i.
// Append only. Never edit an applied step.
//
// AUTOINCREMENT (run_segments.id, run_events.id, event_log.seq) is a known
// Postgres-port blocker. Leave it until the port is scheduled; a migration
// purely for portability has no payoff on SQLite.
func migrations() []string { return nil }

//go:embed schema.sql
var baseSchemaSQL string

// baseSchema returns the full schema at baseVersion as one SQL script.
// migrate makes schema_migrations, so the script leaves it out.
func baseSchema() string { return baseSchemaSQL }
