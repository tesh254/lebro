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

Lebro migrations are forward-only. A binary built for an older schema might not
read newer records; rollback means restore a tested backup or redeploy code
compatible with the migrated schema. Application-owned JSON in metadata, inputs,
and outputs needs its own version field and decoder migration strategy.

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
