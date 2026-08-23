# Product build guides

Each guide documents one complete system built entirely from lebro's public
primitives, with a runnable example under `examples/`. The examples run with no
network, API key, or external service: deterministic stand-ins (scripted
models, fake recognizers, local embedders) occupy the provider seams, and every
guide lists exactly what to swap for production.

The builds share one design rule: isolation, durability, and scope are
enforced by library contracts — policies, schemas, durable stores — not by
scattered glue code in your service.

| Build | Ships like | Example |
| --- | --- | --- |
| [Docs support agent](#docs-support-agent) | Intercom Fin | `examples/docs-support-agent` |
| [Document extraction service](#document-extraction-service) | Reducto / Docsumo | `examples/document-extraction` |
| [Refund copilot with human sign-off](#refund-copilot-with-human-sign-off) | Stripe dispute flow | `examples/refund-approval` |
| [Morning competitive-intel digest](#morning-competitive-intel-digest) | Daily brief bot | `examples/morning-digest` |
| [Internal helpdesk front desk](#internal-helpdesk-front-desk) | Tier-1 IT triage | `examples/helpdesk-router` |
| [Multi-tenant B2B agent platform](#multi-tenant-b2b-agent-platform) | A B2B agent API | `examples/multitenant-platform` |
| [Voice booking line](#voice-booking-line) | A reservations line | `examples/voice-booking` |
| [CI-gated agent releases](#ci-gated-agent-releases) | Braintrust / LangSmith | `examples/ci-gated-releases` |

## Docs support agent

An agent that answers customer questions **only** from your help-center corpus,
and keeps one durable conversation per customer. This is the Intercom-Fin
shape: grounded answers, per-customer memory, and hard guarantees about what
the agent can and cannot read.

Runnable example: [`examples/docs-support-agent`](../examples/docs-support-agent)
(see its README for expected output).

### How it is composed

| Concern | Primitive |
| --- | --- |
| Corpus ingestion | `NewIndexer` over a chunker + embedder + vector store |
| Fixed retrieval scope | `NewRetrievalTool` with a code-owned metadata filter (`visibility: public`) |
| On-corpus behavior | The model answers only from retrieved chunks; an empty result produces a refusal, not a guess |
| Per-customer memory | `AgentConfig.Store` plus `RunInput.ThreadID` — each thread auto-creates and persists every turn |

The scoping guarantee is structural: the retrieval filter is fixed at
construction, is never model-settable, and excludes internal documents no
matter what the model requests. Relevance thresholds keep unrelated-but-public
chunks from producing confident nonsense; when nothing relevant comes back, the
agent declines instead of improvising.

### Production checklist

- Swap `localEmbedder` for `openai.NewEmbedder` (or any other
  `EmbeddingModel`) — the pipeline is unchanged.
- Replace the scripted model with `openai.New`, `anthropic.New`, or
  `gemini.New`.
- Back threads with `SQLiteStore` or `PostgresStore` so customer history
  survives deploys.
- For Slack/Intercom-style inbound traffic, put the same agent behind a
  channel adapter (`examples/channels`); recall older context semantically
  with thread history (`examples/thread-history`).

## Document extraction service

Messy document text in, schema-validated JSON out, behind one HTTP endpoint —
the Reducto/Docsumo shape. Clean documents return validated invoice objects;
malformed ones fail loudly with a typed, machine-readable error instead of
returning garbage with a 200 status.

Runnable example: [`examples/document-extraction`](../examples/document-extraction).

### How it is composed

| Concern | Primitive |
| --- | --- |
| Schema-validated output | `AgentConfig.OutputSchema` with strict JSON Schema; the run fails unless the final value validates |
| One HTTP endpoint | `httpapi.Server.ExposeAgent` serving `POST /agents/{id}/runs` |
| Generated contract | `server.OpenAPI()` renders the full OpenAPI document at `GET /openapi.json` |
| Loud failure | Invalid structured output maps to a public `invalid_output` error body (502), never a silent success |

Because validation happens inside the run boundary, callers get exactly two
outcomes: validated JSON, or a typed error naming what failed. There is no
third state where a client must guess whether `"total": "unreadable"` means
zero dollars.

### Production checklist

- Point the same handler at `http.ListenAndServe` and swap the fixture model
  for a real provider adapter; schema and contract are unchanged.
- Add authentication in `httpapi.ServerConfig.Middleware`; the package ships
  none deliberately.
- Call it with the typed Go client (`httpapi.NewClient`,
  `examples/http-client`) to keep error handling identical in-process and
  remote.
- Extend the same pattern to resumes, receipts, or any document type by
  declaring a new strict output schema on its own agent ID.

## Refund copilot with human sign-off

The agent proposes, a human approves, and the run survives the wait by parking
in a durable suspended state — the Stripe dispute-flow shape. Money moves only
after someone with the right permission resumes the run.

Runnable example: [`examples/refund-approval`](../examples/refund-approval).

### How it is composed

| Concern | Primitive |
| --- | --- |
| Propose, don't act | Linear workflow: propose → await approval → commit |
| Wait for a human | `*SuspendError` plus `SuspendSchema`; resume consent is schema-checked before any step executes |
| Survive the wait | SQLite-backed snapshots; closing and reopening the store proves the suspension outlives the process |
| Human permission | An application gate requires the `refunds:approve` capability before `Resume` is called |
| Bad consent | Malformed resume input returns `ErrInvalidResumeInput` without touching the snapshot |

The capability check lives at your HTTP boundary where authentication already
happens; lebro deliberately ships no identity provider. Everything below that
check — durable state, contract validation, crash-safe resume ordering — is
library-enforced, so an approver clicking "confirm" twice, or a deploy landing
mid-review, cannot double-commit or orphan the run.

### Production checklist

- Wire the capability check into your existing auth middleware instead of a
  helper function; carry approver identity into run metadata for audit.
- Point `commit-refund` at your payment provider; it only executes after a
  valid approval persisted to the snapshot.
- Surface pending approvals by listing `RunStatusSuspended` runs from the
  store.
- Use PostgreSQL storage when multiple processes must see the same suspended
  runs.

## Morning competitive-intel digest

A 06:00 cron fans a job out across news, changelog, and pricing sources in
parallel, joins the results into one brief, and needs no warm process: the
schedule lives in the store, so after any outage the next process to start
fires everything due.

Runnable example: [`examples/morning-digest`](../examples/morning-digest).

### How it is composed

| Concern | Primitive |
| --- | --- |
| 06:00 cron | `Scheduler` plus a persisted `ScheduleRecord` (`0 6 * * *`, skip-if-running concurrency) |
| Parallel fan-out | `StepDefinition.FanOut` with three branches under `MaxParallel` |
| Deterministic join | Branch outputs join in declaration order regardless of completion timing |
| Outage survival | Due schedules reload from the store on every tick; missed occurrences are recorded, bounded by `MaxCatchUp` |

The join step receives a stable array of `{"name","output"}` records, so
downstream summarization is deterministic even though branches finish at
different times. Execution history (`succeeded` / `failed` / `skipped` /
`missed`) is durable, so you can prove what ran while you were asleep.

### Production checklist

- Replace each branch handler with a real fetcher (HTTP scrape, RSS, vendor
  API).
- Insert an agent step after the join to summarize; see
  `workflow-agents-tools`.
- Run `scheduler.Start(ctx)` in production instead of manual ticks; drive
  tests with `scheduler.Tick` and a fixed clock.
- Store schedules in PostgreSQL when more than one scheduler instance runs;
  the concurrency policy prevents duplicate fires.

## Internal helpdesk front desk

One entry point routes every employee request to the right specialist — IT,
HR, facilities — under bounded traversal, with every handoff persisted as an
auditable record. The tier-1 triage shape.

Runnable example: [`examples/helpdesk-router`](../examples/helpdesk-router).

### How it is composed

| Concern | Primitive |
| --- | --- |
| One entry point | `NewNetwork` with a single workflow definition |
| Deterministic routing | `NewRuleRouter`: ordered keyword rules plus a default specialist |
| Specialists | Independent agents registered as `NetworkSpecialist`s |
| Bounded traversal | `MaxHops` caps handoffs; a specialist is never revisited within one run |
| Audit trail | `NetworkRouteRecord`s persisted into the run record, decodable from any store |

Routing is application-owned: rules are plain functions evaluated in
declaration order, so triage behavior is reviewable in a diff. When keyword
rules stop being enough, swap in `NewModelSpecialistRouter` — a model picks the
specialist but is still validated against the configured candidate list, so it
can never invent a destination.

### Production checklist

- Give specialists real tools (password resets, HR lookups); see
  `tools-schema`.
- Route on model judgment with `ModelSpecialistRouter` once categories grow
  beyond keyword-friendly sets.
- Read route records back into your ticketing system; they reconstruct every
  hop without storage migrations.

## Multi-tenant B2B agent platform

Sell the same agent to many customers with per-tenant isolation enforced by
the library at four points, plus wire-level redaction of tool-call arguments —
isolation that does not depend on your glue code remembering every path.

Runnable example: [`examples/multitenant-platform`](../examples/multitenant-platform).

### How it is composed

| Enforcement point | Action | Guarantee |
| --- | --- | --- |
| Agent run start | `ActionAgentRun` | An unlicensed tenant is refused before any model call — provably zero provider spend |
| Every tool call | `ActionToolCall` | Without `tools:call`, the model's tool request never executes; denial names subject, action, resource |
| Workflow run start | `ActionWorkflowRun` | Same gate for workflow surfaces |
| Storage reads/writes | `PolicyStore` | A cross-tenant read hits the policy before the database |

On top of the four points, the default HTTP stream redactor strips tool-call
arguments from streamed deltas: clients learn that a tool ran, never with what.
Identity enters through your authentication middleware as a plain
`lebro.Identity` on the request context; nested work (subagents, workflow
steps, storage calls) authorizes against the same caller automatically.

### Production checklist

- Replace header-based identity middleware with real credential verification;
  the policy sees only the resulting identity.
- Select models and instructions per plan tier with resolvers
  (`examples/request-resolvers`).
- Serve tenants through the typed Go client (`examples/http-client`); denials
  arrive as typed errors matching the in-process sentinels.
- Wrap storage in `NewPolicyStore` in every process that touches tenant data,
  including batch jobs.

## Voice booking line

A phone or in-app voice line: spoken turns transcribe in, run the agent, and
synthesize back out — and booking context survives between a caller's calls.

Runnable example: [`examples/voice-booking`](../examples/voice-booking).

### How it is composed

| Concern | Primitive |
| --- | --- |
| Speech in / speech out | `voice.Session.Turn`: recognize → agent run → synthesize in one call |
| Per-caller memory | `TurnInput.ThreadID` plus `AgentConfig.Store`; later turns load the caller's prior transcript |
| Provider seams | Recognizer and synthesizer interfaces; examples ship fakes |

Memory across calls is ordinary thread persistence: the second call's model
request contains the first call's transcript, so "add a high chair to that"
resolves without the caller repeating themselves. Different callers map to
different threads and never see each other's context.

### Production checklist

- Implement the recognizer/synthesizer interfaces against a real speech
  provider; session wiring is unchanged.
- Give the host agent a booking tool backed by your reservation system so
  replies reflect live inventory.
- Keep one thread ID per caller (phone number or app user) so history
  accumulates correctly.

## CI-gated agent releases

Block a deploy when a prompt or model change regresses real cases, using
content-hashed datasets so comparisons stay honest — the Braintrust/LangSmith
shape. The gate names the exact cases whose pass state moved, which aggregate
scores hide.

Runnable example: [`examples/ci-gated-releases`](../examples/ci-gated-releases).

### How it is composed

| Concern | Primitive |
| --- | --- |
| Honest comparisons | `evals.Dataset.Version()` content-hashes the ordered cases; `Compare` refuses records from different versions |
| Real target | `evals.NewAgentTarget` wraps an actual agent; providers can change without touching the harness |
| Scoring | `ExactMatch` here; add `Regexp` and `ModelScorer` graders as needed |
| Naming regressions | `Compare` lists every `case/scorer` pair that flipped from pass to fail |
| The gate | `comparison.Regressed()` decides block versus approve |

Aggregate means mislead: one improved case can mask one broken case. The gate
works at case granularity, so a prompt rewrite that quietly breaks password
resets blocks the deploy even when the average barely moved.

### Production checklist

- Persist experiments to a shared `evals.Repository` implementation so any CI
  run compares against the last green build.
- Run the gate program in CI with the candidate build; exit non-zero on
  regression.
- Grow datasets from production incidents — every regression you ship becomes
  tomorrow's case.
- See `evals-dataset` for model-graded scorers and offsetting-change detection.
