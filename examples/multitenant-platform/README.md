# Multi-tenant B2B agent platform

One shared agent sold to many customers, with per-tenant isolation the library
enforces at four points — not in your glue code — plus wire-level redaction of
tool-call arguments on streams.

## The four enforcement points

| Point | Action | Guarantee shown |
| --- | --- | --- |
| Agent run start | `ActionAgentRun` | A tenant without a license is refused before any model call (the scripted fixtures are untouched) |
| Every tool call | `ActionToolCall` | Without `tools:call`, the model's tool request never executes; the run fails with a typed `*lebro.PolicyDenial` naming subject, action, resource |
| Workflow run start | `ActionWorkflowRun` | Same gate for workflows |
| Storage reads/writes | `PolicyStore` + `ActionStorageRead`/`Write` | A cross-tenant thread read is denied before it reaches the repository |

On top: streamed tool-call arguments are stripped by the default
`httpapi` redactor, so a client sees that a tool was called but never with what.

## Run

```sh
go run ./examples/multitenant-platform
```

No network or API key required.

## What you should see

- `other` tenant: denied at run start, zero model calls.
- `kim` (acme, no capability): tool call denied with the typed denial printed.
- `ava` (acme, `tools:call`): full answer.
- Cross-tenant storage read: denied at the policy, not the database.
- Streamed run: tool-call identity visible, arguments visible=`false`.

## Swap in production pieces

- Replace header-based `identityMiddleware` with your real authentication; the
  policy only ever sees the resulting `lebro.Identity`.
- Key per-tenant models and instructions with resolvers; see
  `examples/request-resolvers`.
- Serve tenants through the typed Go client; see `examples/http-client`.
