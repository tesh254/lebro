# Use cases

Each use case below is a complete system built entirely from lebro's public
primitives and shipped as a runnable example under `examples/`. Expand a case
for its usage, how it is composed, the key code, real output, and the
production checklist. Every example runs with no network, API key, or external
service: deterministic stand-ins (scripted models, fake recognizers, local
embedders) occupy the provider seams, and each checklist lists exactly what to
swap for production.

The builds share one design rule: isolation, durability, and scope are
enforced by library contracts — policies, schemas, durable stores — not by
scattered glue code in your service.

| Use case | Ships like | Example |
| --- | --- | --- |
| [Docs support agent](#docs-support-agent) | Intercom Fin | `examples/docs-support-agent` |
| [Document extraction service](#document-extraction-service) | Reducto / Docsumo | `examples/document-extraction` |
| [Refund copilot with human sign-off](#refund-copilot-with-human-sign-off) | Stripe dispute flow | `examples/refund-approval` |
| [Morning competitive-intel digest](#morning-competitive-intel-digest) | Daily brief bot | `examples/morning-digest` |
| [Internal helpdesk front desk](#internal-helpdesk-front-desk) | Tier-1 IT triage | `examples/helpdesk-router` |
| [Multi-tenant B2B agent platform](#multi-tenant-b2b-agent-platform) | A B2B agent API | `examples/multitenant-platform` |
| [Voice booking line](#voice-booking-line) | A reservations line | `examples/voice-booking` |
| [CI-gated agent releases](#ci-gated-agent-releases) | Braintrust / LangSmith | `examples/ci-gated-releases` |

<a id="docs-support-agent"></a>
<details>
<summary><strong>Docs support agent</strong> — answers only from your help center, one durable conversation per customer</summary>

An Intercom-Fin-style support bot. Grounded answers, per-customer memory, and
hard guarantees about what the agent can and cannot read.

**Usage**

```sh
go run ./examples/docs-support-agent
```

Full source: [`examples/docs-support-agent`](../examples/docs-support-agent/)
(see its README for expected behavior notes).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| Corpus ingestion | `NewIndexer` over a chunker + embedder + vector store |
| Fixed retrieval scope | `NewRetrievalTool` with a code-owned metadata filter (`visibility: public`) |
| On-corpus behavior | The model answers only from retrieved chunks; an empty result produces a refusal, not a guess |
| Per-customer memory | `AgentConfig.Store` plus `RunInput.ThreadID` — each thread auto-creates and persists every turn |

**Key code** (condensed from `main.go`; see the directory for the full program)

```go
// Retrieval scope is configuration, not a model's choice: this filter is
// fixed at construction and no model input can widen the corpus past it.
retriever := mustValue(lebro.NewVectorRetriever(lebro.VectorRetrieverConfig{
	Embeddings: embeddings, Store: vectorStore, Index: "handbook",
	TopK: 2, CandidateTopK: 4,
	Reranker: mustValue(lebro.NewScorerReranker(keywordScorer{})),
	Filter: lebro.VectorMetadataFilter{
		Match: map[string]json.RawMessage{"visibility": json.RawMessage(`"public"`)},
	},
}))
tool := mustValue(lebro.NewRetrievalTool(lebro.RetrievalToolConfig{
	ID: "search_handbook", Retriever: retriever,
	Description: "Search the customer handbook for passages relevant to a question.",
	TopK: 2, MaxTopK: 4,
}))
agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
	Definition: lebro.AgentDefinition{ID: "docs-support", Tools: []lebro.ToolID{"search_handbook"}},
	Model:      model, Tools: registry, Store: store, MaxSteps: 4,
}))

// One shared agent; per-customer state lives under the caller's thread ID.
result, err := agent.Run(ctx, lebro.RunInput{
	ThreadID: lebro.ThreadID("customer-acme-1"),
	Messages: []lebro.Message{{Role: lebro.RoleUser, Content: question}},
})
```

**Expected output**

```text
indexed refunds (1 chunk(s))
indexed shipping (1 chunk(s))
indexed margins (1 chunk(s))

[customer-acme-1] What is the refund window?
[customer-acme-1] According to policies/refunds.md: Refund policy: customers may request a full refund within 30 days of purc...

[customer-acme-1] How long does standard delivery take?
[customer-acme-1] According to policies/shipping.md: Shipping policy: standard delivery takes five business days. Express del...

[customer-globex-9] What are your supplier margin targets?
[customer-globex-9] The public handbook says nothing about that, so I cannot help here.
customer-acme-1 persisted messages: 8
customer-globex-9 persisted messages: 4
```

**Production checklist**

- Swap `localEmbedder` for `openai.NewEmbedder` (or any other
  `EmbeddingModel`) — the pipeline is unchanged.
- Replace the scripted model with `openai.New`, `anthropic.New`, or
  `gemini.New`.
- Back threads with `SQLiteStore` or `PostgresStore` so customer history
  survives deploys.
- For Slack/Intercom-style inbound traffic, put the same agent behind a
  channel adapter (`examples/channels`); recall older context semantically
  with thread history (`examples/thread-history`).

</details>

<a id="document-extraction-service"></a>
<details>
<summary><strong>Document extraction service</strong> — messy documents in, validated JSON out, loud failures</summary>

A Reducto/Docsumo-style extraction endpoint. Clean documents return validated
invoice objects; malformed ones fail loudly with a typed, machine-readable
error instead of returning garbage with a 200 status.

**Usage**

```sh
go run ./examples/document-extraction
```

Full source: [`examples/document-extraction`](../examples/document-extraction/).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| Schema-validated output | `AgentConfig.OutputSchema` with strict JSON Schema; the run fails unless the final value validates |
| One HTTP endpoint | `httpapi.Server.ExposeAgent` serving `POST /agents/{id}/runs` |
| Generated contract | `server.OpenAPI()` renders the full OpenAPI document at `GET /openapi.json` |
| Loud failure | Invalid structured output maps to a public `invalid_output` error body (502), never a silent success |

Because validation happens inside the run boundary, callers get exactly two
outcomes: validated JSON, or a typed error naming what failed.

**Key code**

```go
agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
	Definition:     lebro.AgentDefinition{ID: "invoice-parser"},
	Model:          model,
	SchemaCompiler: lebrojsonschema.NewCompiler(),
	OutputSchema: &lebro.ModelOutputSchema{
		Name:   "invoice",
		Schema: json.RawMessage(invoiceSchema), // required fields, enums, additionalProperties:false
		Strict: true,
	},
}))
server := httpapi.NewServer(httpapi.ServerConfig{Title: "invoice-extraction"})
must(server.ExposeAgent(agent)) // POST /agents/invoice-parser/runs
```

**Expected output**

```text
== a clean extraction returns validated invoice JSON ==
200 OK
{"run_id":"agent-run-0001","status":"succeeded","content":"","structured_output":{"currency":"KES","invoice_id":"INV-2043","total_cents":4198000,"vendor":"Acme Supplies Ltd"}}

== a malformed extraction fails loudly, not silently ==
502 Bad Gateway
{"error":{"code":"invalid_output","message":"the run produced output that failed schema validation"}}
```

The run also prints the generated OpenAPI path list, including
`/agents/{id}/runs`.

**Production checklist**

- Point the same handler at `http.ListenAndServe` and swap the fixture model
  for a real provider adapter; schema and contract are unchanged.
- Add authentication in `httpapi.ServerConfig.Middleware`; the package ships
  none deliberately.
- Call it with the typed Go client (`httpapi.NewClient`,
  `examples/http-client`) to keep error handling identical in-process and
  remote.
- Extend the same pattern to resumes, receipts, or any document type by
  declaring a new strict output schema on its own agent ID.

</details>

<a id="refund-copilot-with-human-sign-off"></a>
<details>
<summary><strong>Refund copilot with human sign-off</strong> — proposes, parks durably, commits only after approval</summary>

The Stripe dispute-flow shape: the agent proposes, a human approves, and the
run survives the wait in a durable suspended state. Money moves only after
someone with the right permission resumes the run.

**Usage**

```sh
go run ./examples/refund-approval
```

Full source: [`examples/refund-approval`](../examples/refund-approval/).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| Propose, don't act | Linear workflow: propose → await approval → commit |
| Wait for a human | `*SuspendError` plus `SuspendSchema`; resume consent is schema-checked before any step executes |
| Survive the wait | SQLite-backed snapshots; closing and reopening the store proves the suspension outlives the process |
| Human permission | An application gate requires the `refunds:approve` capability before `Resume` is called |
| Bad consent | Malformed resume input returns `ErrInvalidResumeInput` without touching the snapshot |

**Key code**

```go
// The suspending step publishes its expectation; Resume validates against it.
Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return nil, &lebro.SuspendError{Signal: lebro.SuspendSignal{
		StepID:   "await-approval",
		Contract: json.RawMessage(`{"approved":true,"approver":"dana"}`),
		Payload:  json.RawMessage(`{"pending":"human approval with refunds:approve"}`),
	}}
})

// The application's human-sign-off boundary, checked before Resume runs.
func authorizeApprover(identity lebro.Identity) error {
	if !identity.HasCapability(approveRefunds) { // Capability("refunds:approve")
		return fmt.Errorf("subject %q lacks %s", identity.Subject, approveRefunds)
	}
	return nil
}
resumed, err := wf.Resume(ctx, lebro.WorkflowResumeInput{
	RunID: suspended.ID, Input: json.RawMessage(`{"approved":true,"approver":"dana"}`),
})
```

**Expected output**

```text
proposed refund for ORD-77; status: suspended
suspended at "await-approval"; resume contract: {"approved":true,"approver":"dana"}
support-agent refused (subject "support-agent" lacks refunds:approve); run untouched
invalid consent rejected: lebro: workflow step_failed: lebro: lebro: workflow resume input invalid: lebro: resume input validation failed at /approved: value must be true
resumed by dana; status: succeeded
final output: {"amount_cents":8400,"approver":"dana","refunded":true}
```

**Production checklist**

- Wire the capability check into your existing auth middleware instead of a
  helper function; carry approver identity into run metadata for audit.
- Point `commit-refund` at your payment provider; it only executes after a
  valid approval persisted to the snapshot.
- Surface pending approvals by listing `RunStatusSuspended` runs from the
  store.
- Use PostgreSQL storage when multiple processes must see the same suspended
  runs.

</details>

<a id="morning-competitive-intel-digest"></a>
<details>
<summary><strong>Morning competitive-intel digest</strong> — 06:00 cron fans out across sources, joins one brief, survives outages</summary>

At 06:00 a persisted schedule fans a job out across news, changelog, and
pricing sources in parallel and joins the results into one brief. No warm
process is required: after any outage the next process to start fires what is
due.

**Usage**

```sh
go run ./examples/morning-digest
```

Full source: [`examples/morning-digest`](../examples/morning-digest/).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| 06:00 cron | `Scheduler` plus a persisted `ScheduleRecord` (`0 6 * * *`, skip-if-running concurrency) |
| Parallel fan-out | `StepDefinition.FanOut` with three branches under `MaxParallel` |
| Deterministic join | Branch outputs join in declaration order regardless of completion timing |
| Outage survival | Due schedules reload from the store on every tick; missed occurrences are recorded, bounded by `MaxCatchUp` |

**Key code**

```go
// Three independent branches run concurrently and join in declaration order.
FanOut: &lebro.FanOut{
	MaxParallel: 3,
	FailurePolicy: lebro.FanOutCollectAll,
	Branches: []lebro.FanOutBranch{
		{Name: "news", Steps: fetchNewsSteps},
		{Name: "changelog", Steps: fetchChangelogSteps},
		{Name: "pricing", Steps: fetchPricingSteps},
	},
}

// Persist everything a restart needs, then let a fresh scheduler fire it.
store.Schedules().SaveSchedule(ctx, lebro.ScheduleRecord{
	ID: "morning-digest", WorkflowID: "competitor-digest",
	Spec: "0 6 * * *", Concurrency: lebro.ConcurrencySkip,
	Input: json.RawMessage(`{"date":"2026-08-12"}`), NextFireAt: &due,
})
scheduler := lebro.NewScheduler(lebro.SchedulerConfig{
	Store: reopened, Resolver: lebro.WorkflowMap{"competitor-digest": digest},
})
tick, _ := scheduler.Tick(ctx, now) // fires every overdue schedule
```

**Expected output**

```text
schedule persisted at 2026-08-12T07:00:00Z; process exits
tick fired 1 schedule(s)
digest run agent-run-0001: succeeded
MORNING COMPETITOR BRIEF

news (industry news)
  - Rival shipped on-prem deployment option
  - Rival raised Series C

changelog (product changelog)
  - Rival added SSO on mid-tier plans
  - Rival deprecated legacy webhook format

pricing (pricing page)
  - Rival mid-tier price moved from $49 to $59 per seat

next fire: 2026-08-13T06:00:00Z
```

**Production checklist**

- Replace each branch handler with a real fetcher (HTTP scrape, RSS, vendor
  API).
- Insert an agent step after the join to summarize; see
  `workflow-agents-tools`.
- Run `scheduler.Start(ctx)` in production instead of manual ticks; drive
  tests with `scheduler.Tick` and a fixed clock.
- Store schedules in PostgreSQL when more than one scheduler instance runs;
  the concurrency policy prevents duplicate fires.

</details>

<a id="internal-helpdesk-front-desk"></a>
<details>
<summary><strong>Internal helpdesk front desk</strong> — one entry point routes IT / HR / facilities with an audit trail</summary>

Every employee request enters through one entry point, a deterministic router
hands it to the right specialist under bounded traversal, and every handoff is
persisted as an auditable record.

**Usage**

```sh
go run ./examples/helpdesk-router
```

Full source: [`examples/helpdesk-router`](../examples/helpdesk-router/).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| One entry point | `NewNetwork` with a single workflow definition |
| Deterministic routing | `NewRuleRouter`: ordered keyword rules plus a default specialist |
| Specialists | Independent agents registered as `NetworkSpecialist`s |
| Bounded traversal | `MaxHops` caps handoffs; a specialist is never revisited within one run |
| Audit trail | `NetworkRouteRecord`s persisted into the run record, decodable from any store |

**Key code**

```go
router := mustValue(lebro.NewRuleRouter([]lebro.RouteRule{
	{SpecialistID: "it", Match: mentions("vpn", "password", "laptop", "login")},
	{SpecialistID: "hr", Match: mentions("leave", "benefits", "payroll")},
	{SpecialistID: "facilities", Match: mentions("desk", "chair", "office")},
}, "it"))
network := mustValue(lebro.NewNetwork(lebro.NetworkConfig{
	Definition: lebro.WorkflowDefinition{ID: "helpdesk-front-desk"},
	Router:     router,
	Specialists: []lebro.NetworkSpecialist{
		{ID: "it", Workflow: itAgent, Description: "Accounts, devices, access."},
		{ID: "hr", Workflow: hrAgent, Description: "Leave, benefits, people ops."},
		{ID: "facilities", Workflow: facilitiesAgent, Description: "Building issues."},
	},
	MaxHops: 2, // one handoff plus the router's completion check
	Store:   store,
}))
result, err := network.Run(ctx, lebro.RunInput{Messages: ticket})
```

**Expected output**

```text
TCK-1 routed to it (1 hop(s))
  reply: Reset the credentials and confirm.
TCK-2 routed to hr (1 hop(s))
  reply: Unused leave carries over automatically; nothing to file.
TCK-3 routed to facilities (1 hop(s))
  reply: A replacement chair is scheduled for tomorrow.
```

**Production checklist**

- Give specialists real tools (password resets, HR lookups); see
  `tools-schema`.
- Route on model judgment with `NewModelSpecialistRouter` once categories grow
  beyond keyword-friendly sets.
- Read route records back into your ticketing system; they reconstruct every
  hop without storage migrations.

</details>

<a id="multi-tenant-b2b-agent-platform"></a>
<details>
<summary><strong>Multi-tenant B2B agent platform</strong> — isolation enforced at four points, arguments redacted on the wire</summary>

One shared agent sold to many customers, with per-tenant isolation the library
enforces — not glue code that must remember every path.

**Usage**

```sh
go run ./examples/multitenant-platform
```

Full source: [`examples/multitenant-platform`](../examples/multitenant-platform/).

**How it is composed**

| Enforcement point | Action | Guarantee |
| --- | --- | --- |
| Agent run start | `ActionAgentRun` | An unlicensed tenant is refused before any model call — provably zero provider spend |
| Every tool call | `ActionToolCall` | Without `tools:call`, the model's tool request never executes; denial names subject, action, resource |
| Workflow run start | `ActionWorkflowRun` | Same gate for workflow surfaces |
| Storage reads/writes | `PolicyStore` | A cross-tenant read hits the policy before the database |

On top of the four points, the default HTTP stream redactor strips tool-call
arguments from streamed deltas: clients learn that a tool ran, never with what.

**Key code**

```go
type platformPolicy struct{ allowedTenant string }

func (p platformPolicy) Authorize(_ context.Context, identity lebro.Identity, action lebro.Action, resource lebro.Resource) lebro.Decision {
	switch action {
	case lebro.ActionAgentRun, lebro.ActionWorkflowRun, lebro.ActionNetworkRun:
		if identity.Tenant != p.allowedTenant {
			return lebro.Deny(fmt.Sprintf("tenant %q is not licensed", identity.Tenant))
		}
	case lebro.ActionToolCall:
		if identity.Tenant != p.allowedTenant || !identity.HasCapability(callTools) {
			return lebro.Deny(fmt.Sprintf("missing %s capability", callTools))
		}
	case lebro.ActionStorageRead, lebro.ActionStorageWrite:
		if identity.Tenant != p.allowedTenant {
			return lebro.Deny("tenant may not touch this store")
		}
	}
	return lebro.Allow()
}

guarded := lebro.NewPolicyStore(store, policy)          // storage point
ctx := lebro.WithIdentity(ctx, lebro.Identity{Subject: "ava", Tenant: "acme"})
_, err := agent.Run(ctx, lebro.RunInput{Messages: ticket}) // denied tenants never reach the model
```

Identity enters through your authentication middleware as a plain
`lebro.Identity`; nested work authorizes against the same caller automatically.

**Expected output**

```text
other tenant run: denied before any model call (fixtures left: 2)
kim without tools:call: tool call denied (tool.call on "weather.lookup")
ava with tools:call: Nairobi is 24.5C.
cross-tenant thread read: denied before reaching the store
streamed tool call weather.lookup: arguments visible=false
```

**Production checklist**

- Replace header-based identity middleware with real credential verification;
  the policy only ever sees the resulting identity.
- Select models and instructions per plan tier with resolvers
  (`examples/request-resolvers`).
- Serve tenants through the typed Go client (`examples/http-client`); denials
  arrive as typed errors matching the in-process sentinels.
- Wrap storage in `NewPolicyStore` in every process that touches tenant data,
  including batch jobs.

</details>

<a id="voice-booking-line"></a>
<details>
<summary><strong>Voice booking line</strong> — spoken turns in and out, booking memory across calls</summary>

A reservations line: spoken turns transcribe in, run the agent, and synthesize
a reply back out — and booking context survives between a caller's calls.

**Usage**

```sh
go run ./examples/voice-booking
```

Full source: [`examples/voice-booking`](../examples/voice-booking/).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| Speech in / speech out | `voice.Session.Turn`: recognize → agent run → synthesize in one call |
| Per-caller memory | `TurnInput.ThreadID` plus `AgentConfig.Store`; later turns load the caller's prior transcript |
| Provider seams | Recognizer and synthesizer interfaces; the example ships fakes |

Memory across calls is ordinary thread persistence: the second call's model
request contains the first call's transcript, so "add a high chair to that"
resolves without the caller repeating themselves.

**Key code**

```go
session := mustValue(voice.NewSession(voice.SessionConfig{
	Voice: voice.Voice{Recognizer: recognizer, Synthesizer: synthesizer},
	Agent: hostAgent, // bound to a Store so threads persist
}))

audio := make(chan voice.AudioChunk, 2)
audio <- voice.AudioChunk{Data: []byte(utterance)}
audio <- voice.AudioChunk{Final: true}
close(audio)

// ThreadID keys the caller's durable conversation across calls.
result, err := session.Turn(ctx, voice.TurnInput{
	Audio: audio, ThreadID: lebro.ThreadID(callerID),
}, func(voice.AudioChunk) error { return nil })
```

**Expected output**

```text
[caller-555] heard: "Book a table for two on Friday at seven"
[caller-555] said:  Table for two, Friday at seven, under this number - confirmed.

[caller-555] heard: "Add a high chair to that booking"
[caller-555] said:  Done - a high chair is added to your Friday table for two.

[caller-777] heard: "Do you have outdoor seating"
[caller-777] said:  Yes, outdoor seating is first come from five o'clock.
caller-555 persisted turns: 4
caller-777 persisted turns: 2
```

**Production checklist**

- Implement the recognizer/synthesizer interfaces against a real speech
  provider; session wiring is unchanged.
- Give the host agent a booking tool backed by your reservation system so
  replies reflect live inventory.
- Keep one thread ID per caller (phone number or app user) so history
  accumulates correctly.

</details>

<a id="ci-gated-agent-releases"></a>
<details>
<summary><strong>CI-gated agent releases</strong> — content-hashed evals, deploy blocked when real cases regress</summary>

A Braintrust/LangSmith-style release gate: a prompt or model change runs
against a versioned dataset, and the deploy is blocked when cases regress.
Dataset versions are content hashes, so comparisons always describe the same
questions, and `Compare` names the exact cases that moved.

**Usage**

```sh
go run ./examples/ci-gated-releases
```

Full source: [`examples/ci-gated-releases`](../examples/ci-gated-releases/).

**How it is composed**

| Concern | Primitive |
| --- | --- |
| Honest comparisons | `evals.Dataset.Version()` content-hashes the ordered cases; `Compare` refuses records from different versions |
| Real target | `evals.NewAgentTarget` wraps an actual `*lebro.Agent` |
| Scoring | `ExactMatch` here; add `Regexp` and `ModelScorer` graders as needed |
| Naming regressions | `Compare` lists every `case/scorer` pair whose pass state flipped |
| The gate | `comparison.Regressed()` decides block versus approve |

Aggregate means mislead: one improved case can mask one broken case. The gate
works at case granularity.

**Key code**

```go
dataset := evals.Dataset{ID: "support-bot-regression", Cases: []evals.Case{
	{ID: "refund-window", Input: json.RawMessage(`"How long do I have to request a refund?"`),
	 Expected: "You can request a refund within 30 days."},
	// ...
}}
experiment := mustValue(evals.New(evals.ExperimentConfig{
	Name: "candidate", Dataset: dataset,
	Target:  evals.NewAgentTarget(candidateAgent),
	Scorers: []evals.Scorer{exactMatch}, Repository: repository,
}))
baselineRecord, baselineCases, _ := experiment.Run(ctx)

comparison, err := evals.Compare(baselineRecord, candidateRecord, baselineCases, candidateCases)
if comparison.Regressed() {
	fmt.Println("DEPLOY BLOCKED") // exit non-zero in CI
}
```

**Expected output**

```text
dataset support-bot-regression version 17714c54eccd

baseline (support-bot-regression-17714c54eccd-...): 3/3 passed
candidate (support-bot-regression-17714c54eccd-...): 2/3 passed
REGRESSED password-reset/exact_match: expected "We will email you a reset link shortly.", got "Please contact support so an agent can restore your access."

DEPLOY BLOCKED: regressions against dataset support-bot-regression
```

Experiment IDs embed a timestamp and vary per run.

**Production checklist**

- Persist experiments to a shared `evals.Repository` implementation so any CI
  run compares against the last green build.
- In CI, run the gate with the candidate build and exit non-zero on
  regression.
- Grow datasets from production incidents — every regression you ship becomes
  tomorrow's case.
- See `evals-dataset` for model-graded scorers and offsetting-change
  detection.

</details>
