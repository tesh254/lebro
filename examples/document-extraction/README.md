# Document extraction service

A Reducto/Docsumo-style extraction endpoint: messy document text goes in at one
HTTP route, schema-validated invoice JSON comes out — and a malformed
extraction fails loudly with a typed public error instead of returning garbage.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| Schema-validated output | `AgentConfig.OutputSchema` (strict JSON Schema) — the run fails unless the final value validates |
| One HTTP endpoint | `httpapi.Server.ExposeAgent` → `POST /agents/{id}/runs` with an auto-generated OpenAPI contract at `GET /openapi.json` |
| Loud failure | An invalid structured output surfaces as `invalid_output` (502), never as a silent 200 |

## Run

```sh
go run ./examples/document-extraction
```

No network or API key is required: the model is a deterministic scripted
stand-in that emits a valid invoice for readable documents and schema-breaking
fields for unreadable ones.

## What you should see

- `200 OK` with `"status":"succeeded"` and the validated invoice object
  (`invoice_id`, `vendor`, `total_cents`, `currency`).
- `502 Bad Gateway` with `{"error":{"code":"invalid_output",...}}` when the
  document is unreadable — the failure is typed, public, and machine-readable.
- The generated OpenAPI paths, including `/agents/{id}/runs`.

## Swap in production pieces

- `scriptedModel` → any provider adapter; the output schema and HTTP contract
  are unchanged.
- Add authentication in `httpapi.ServerConfig.Middleware`; the package ships
  none deliberately.

For the typed Go client against this same API, see `examples/http-client`.
