# Migration guide

Run a store migration before serving traffic that uses it. `MemoryStore` has no
schema migration. SQLite and Postgres stores, plus their vector-store variants,
own their schema migrations and expose `Migrate(context.Context)`.

```go
store, err := lebro.NewSQLiteStore("lebro.db")
if err != nil { return err }
defer store.Close()
if err := store.Migrate(ctx); err != nil { return err }
```

## Deployment procedure

1. Read the target release's `CHANGELOG.md` and this guide.
2. Back up durable SQLite/Postgres data and verify restore before production.
3. Deploy one instance or a dedicated migration job with a bounded context.
4. Run `Migrate` for each configured store, including vector stores.
5. Roll out application instances only after migration succeeds; monitor typed
   migration errors and connection-pool pressure.

Migrations are idempotent and transactional. SQLite uses `BEGIN IMMEDIATE` and
Postgres takes a transaction-scoped advisory lock, so concurrent migration
attempts serialize. Do not hand-edit `schema_migrations`,
`vector_schema_migrations`, `PRAGMA user_version`, or runtime tables: those are
implementation details, not a public schema API.

### PostgreSQL schemas

`PostgresStoreOptions.Schema` optionally places every Lebro table and its
`schema_migrations` ledger in one validated PostgreSQL schema. Empty keeps the
database's existing `search_path`. The store applies the configured search path
to every pooled connection, creates the schema when needed, and uses it for
migration and repository queries. Supply only a simple PostgreSQL identifier;
invalid values are rejected before opening a connection.

```go
store, err := lebro.NewPostgresStore(dsn, lebro.PostgresStoreOptions{
    Schema: "lebro_runtime",
})
```

Two stores may safely use different schemas in one database; their tables and
migration ledgers do not collide. This option is for Lebro's built-in store.
An application-owned RuntimeStore instead owns its own migrations and schema.

Lebro migrations are forward-only. A binary built for an older schema might not
read newer records; rollback means restore a tested backup or redeploy code
compatible with the migrated schema. Application-owned JSON in metadata, inputs,
and outputs needs its own version field and decoder migration strategy.

## Reasoning transcript records

Reasoning support adds optional fields inside the existing serialized `Message`
value: displayable reasoning text and opaque provider replay details. Memory,
SQLite, and Postgres stores therefore need no SQL migration for this release.
Existing messages decode with zero reasoning fields; new messages keep the full
provider replay payload in the existing `messages.message` JSON column. Run
`Migrate` as part of the normal deployment procedure, but no new schema version
is introduced solely for reasoning.

## Durable run records

MAD-83 observability support adds three append-only tables to the SQLite and
Postgres schemas (`run_events`, `model_attempts`, `tool_executions`) plus
indexes; `MemoryStore` needs no schema. Run `Migrate` as part of the normal
deployment procedure. The tables are independent of `threads` on purpose, so
failed runs can persist diagnostics before any transcript exists. Records are
written only by stores that opt into `ObservabilityRepositories`; existing
databases gain empty tables and nothing else changes. See
`docs/run-records.md` for retention and redaction guidance.

## Durable workflows and schedules

Persisted workflow records retain caller-supplied definition/version references;
the executor does not reinterpret them. Keep old workflow definitions available
until outstanding runs, suspensions, and schedule executions complete. Test a
resume after every workflow-definition or schema change. Schedule definitions
survive restart, but operations must set a concurrency policy appropriate for
their side effects.

## HTTP and MCP clients

Before deploying an HTTP client and server independently, call
`httpapi.Client.CheckCompatibility`. It compares the major version published as
`x-lebro-contract-version` in `/openapi.json`; different majors are incompatible.
Additive fields can appear in a compatible version, so JSON decoders must ignore
unknown fields.

MCP uses the protocol version declared by the `mcp` package. Upgrade its client
and server together when that protocol or a tool's declared input/output schema
changes. Preserve tool IDs and schema compatibility during staged deployments.
