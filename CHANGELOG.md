# Changelog

All notable changes to this project are documented in this file.

## Unreleased

### Added

- Tool-call and JSON-schema structured-output support in the OpenAI-compatible
  chat-completions adapter (`openai`). `ModelRequest.Tools` maps to function
  tools, assistant tool-call turns and tool results map to `tool_calls` and
  `tool_call_id` continuations, `ModelRequest.OutputSchema` maps to
  `response_format: json_schema`, and streaming surfaces complete tool calls
  before the terminal delta. The adapter previously rejected every request
  carrying tools or an output schema. Request-body keys derived from the
  neutral protocol (`model`, `messages`, `stream`, `tools`, `response_format`)
  are now reserved and cannot be overridden through `ModelRequest.Extension`.

- Eight product-shaped example builds, each a standalone runnable directory
  with its own README and an adoption guide in `docs/product-builds.md`:
  `docs-support-agent` (fixed-scope RAG support bot with per-customer durable
  threads), `document-extraction` (schema-validated extraction behind one HTTP
  endpoint with typed loud failures), `refund-approval` (suspend/resume refund
  workflow gated by an approver capability), `morning-digest` (persisted cron
  with parallel fan-out joined into one brief), `helpdesk-router` (bounded
  triage network with auditable route records), `multitenant-platform` (policy
  enforcement at all four authorization points plus streamed tool-call argument
  redaction), `voice-booking` (voice turns with per-caller thread memory), and
  `ci-gated-releases` (content-hashed eval dataset where `Compare` names the
  exact regressed cases). Every example is deterministic, needs no network or
  API key, and imports no internal package, so a copied build compiles against
  the public module alone.

- Publishing notes: `docs/releasing.md` describes first-tag indexing on
  `pkg.go.dev`, including the request-indexing fallback and when to swap the
  README's relative links for versioned pkg.go.dev URLs.

- Recursive and sliding-window RAG chunkers. `NewRecursiveChunker` preserves
  text while preferring paragraph, line, then word boundaries before a rune
  fallback; `NewSlidingWindowChunker` publishes existing rune-safe fixed-window
  behavior under an explicit strategy name. Both preserve document validation,
  stable chunk IDs, provenance, metadata isolation, cancellation, and UTF-8
  rejection. `examples/rag-chunkers` demonstrates both strategies.

- Bounded agent networks. `Network` coordinates named specialist workflows via
  a provider-neutral router with explicit completion, required task and output
  handoffs,
  hop/deadline budgets, cycle detection, identity and policy propagation, typed
  route failures, route-decision events, and durable route records through the
  existing workflow-run storage contracts. Memory, SQLite, and Postgres require
  no migration. `examples/agent-network` demonstrates one deterministic route.

- Root SDK API is organized into domain files (`agent.go`, `network.go`,
  `storage.go`, `workflow.go`, `tools.go`, and related files); `api_types.go`
  holds shared aliases and constants. Public symbols and the
  `github.com/tesh254/lebro` import path remain unchanged.

- Voice integration points. A new optional `voice` package adds optional speech
  input and output around an agent without changing core orchestration: a spoken
  utterance is transcribed to a canonical user turn, the agent runs through the
  ordinary streaming pipeline, and the final reply is synthesized back to audio.
  It is off by default — nothing in the root module imports it, and the root
  module gains no provider dependency. The package supplies the provider-neutral
  edges and leaves the provider-specific work to optional adapters that implement
  `Recognizer` (speech-to-text) and/or `Synthesizer` (text-to-speech); a `Voice`
  bundles the two optional halves. A `Session`, built with `NewSession`, drives
  one voice turn end to end through `Turn` (or the separate `Transcribe` and
  `Synthesize` halves), starting the run from the final transcript via
  `TranscriptMessage` and projecting the reply with `AssistantText`. Recognition
  and synthesis stream with cancellation through `RecognitionStream` and
  `SynthesisStream`, mirroring the agent runtime's stream contract; cancelling
  the context stops the active transcription, run, or synthesis and joins its
  goroutine. Voice provider failures are a distinct `*VoiceError` (kinds
  `Recognition`, `Synthesis`, `Unsupported`, matchable with `ErrRecognition`,
  `ErrSynthesis`, `ErrUnsupported`) and are never confused with an agent
  `*AgentError`. The public surface adds the `voice` package (`Session`,
  `SessionConfig`, `Voice`, `Recognizer`, `Synthesizer`, `Speaker`, `AudioChunk`,
  `AudioFormat`, `Transcript`, `SynthesisRequest`, `TurnInput`, `TurnResult`,
  `RecognitionStream`, `SynthesisStream`, `VoiceError`, and their constructors);
  no new module dependency is introduced.
- Messaging channel adapters. A new optional `channels` package connects agents
  to conversational platforms: a platform delivers an inbound message to a
  webhook, the agent runs through the ordinary streaming pipeline, and the
  streamed reply is delivered back to the same conversation. It is off by
  default — nothing in the root module imports it, and the root module gains no
  provider dependency. The package supplies the provider-neutral edges and
  leaves the platform-specific edges to an `Adapter` (`Platform`, `Verify`,
  `Decode`, `Send`); `NewWebhookAdapter` is a generic HMAC-SHA256 webhook
  adapter that needs no platform SDK. A `Server`, built with `NewServer` and
  populated with `ExposeAgent(agent, adapters...)`, routes each agent-adapter
  pair at `/agents/{id}/channels/{platform}/webhook`. An inbound message maps
  deterministically to a durable thread through a `ThreadMapper`
  (`NamespaceThreadMapper` by default), so every message in one conversation
  lands on one persisted transcript; the sender is mapped onto a
  `lebro.Identity` and carried on the run context so a configured `Policy`
  authorizes the run, and inbound content is always a user turn. Redelivery is
  made safe by a `Deduplicator`, scoped per agent-platform route:
  `StoreDeduplicator` persists a marker per key through a `Store` (the default
  whenever a store is configured) so redelivery is recognized across a restart,
  and `MemoryDeduplicator` keeps a bounded in-process window. The
  webhook handler returns status codes a platform's retry logic can act on. The
  public surface adds the `channels` package (`Server`, `Config`, `ExposeAgent`,
  `Adapter`, `InboundMessage`, `OutboundMessage`, `ChannelIdentity`,
  `ConversationRef`, `TextFormat`, `ThreadMapper`, `NamespaceThreadMapper`,
  `Deduplicator`, `MemoryDeduplicator`, `StoreDeduplicator`, `NewWebhookAdapter`,
  and their config types); no new module dependency is introduced.
- Local Studio-style developer UI. A new optional `studio` package serves a
  local UI for exercising agents, tools, workflows, threads, and run traces
  without writing one-off debugging programs. It is off by default: nothing in
  the root module imports it, and the UI is unreachable until a caller builds
  `Studio.Handler` or calls `studio.Start`, so UI state is never a runtime
  requirement. A `Studio`, built with `studio.New`, composes the surfaces lebro
  already has rather than reimplementing them — agent runs, workflow runs,
  streaming, and thread reads are served by `httpapi` mounted under `/api`, and
  ordered run events (the run, step, model, and tool spans that record what
  happened and in what order, including tool calls, their results, and the path
  a workflow took) are read from an observability `Repository` through the new
  `studio.TraceLister` contract, which `obsv.MemoryRepository` satisfies with no
  adapter. Studio adds read-only `GET /api/studio/traces` and `GET
  /api/studio/traces/{id}`; the trace-detail route returns a run's spans as a
  timeline ordered by start time so a client renders the events top to bottom.
  The web UI is served as a static bundle embedded at build time, with a
  placeholder page when no bundle is present, so a from-source build still
  serves a usable API. `studio.Start` owns a listener and shuts down cleanly on
  context cancellation. The public surface adds the `studio` package (`Config`,
  `Studio`, `New`, `Start`, `TraceLister`, and the trace response types); no new
  module dependency is introduced. The developer UI itself lives in the separate
  `lebro-studio` project.
- Human approval gates for protected workflow steps. A new `ApprovalGate`,
  built with `NewApprovalGate`, wraps a protected `StepHandler` (typically a
  `ToolStep`) in two generated steps: a request step that suspends the run with
  a typed `ApprovalRequest` (action, resource, reason, expiry, and the reviewed
  arguments) and a guard step that resumes only with an `ApprovalDecision` and
  invokes the protected handler solely on approval. The gate reuses the existing
  durable suspend/resume machinery — the request is the suspend payload and the
  decision is the resume input — so a pending approval survives a process
  restart, and reject, approve, timeout, and invalid-decision outcomes are all
  recorded through the existing run events. The guard loads the authoritative
  request from the durable snapshot and requires the decision to echo it exactly,
  so an approved resume cannot execute arguments the approver never reviewed; the
  decision is validated against the requirement's `DecisionSchema` (compiled by
  the workflow's `SchemaCompiler`) before any protected work runs. Expiry is
  enforced deterministically at resume by comparing the decision against the
  request's TTL-derived `ExpiresAt` (a zero TTL never expires) and is evaluated
  before the approval outcome, so a late decision is always a timeout; no
  background timer is introduced. A denial fails the run with
  `ErrApprovalRejected`, a late decision with `ErrApprovalExpired`, and an
  unusable, schema-invalid, or tampered decision with
  `ErrApprovalInvalidDecision`. To let a step suspend for free-form input like an
  approval decision, the workflow executor now treats an empty suspend contract
  as "no pinned resume value" and validates only the resume input against the
  `SuspendSchema`; every existing caller publishes a non-empty contract and is
  unaffected. The public surface adds `ApprovalGate`, `NewApprovalGate`,
  `ApprovalRequest`, `ApprovalDecision`, `ApprovalRequirement`, and the three
  sentinels; no new module dependency is introduced.
- Durable schedules and recurring workflow runs. A new `Scheduler` fires
  persisted schedules whose next fire time has arrived, reusing
  `LinearWorkflow.Run` for each execution, so recurring work survives a process
  restart: due schedules are reloaded from the `Store` on every tick rather than
  held in memory. A `ScheduleRecord` carries a cron or `@every` spec (compiled by
  the standard-library-only `ParseCronSpec` — five fields plus fixed intervals),
  a `ConcurrencyPolicy` (`ConcurrencyAllow` or `ConcurrencySkip`), the workflow
  input, and the persisted `NextFireAt`/`LastFireAt`; each fire appends a
  `ScheduleExecutionRecord` to durable history with a `succeeded`, `failed`,
  `skipped`, or `missed` status, so overlapping runs dropped by the concurrency
  policy and fires elapsed during an outage are both visible after the fact.
  `Scheduler.Tick(ctx, now)` is the deterministic core (testable with a fixed
  `Clock`); `Scheduler.Start`/`Stop` wrap it in a background loop. Two new
  repositories, `ScheduleRepository` and `ScheduleExecutionRepository`, join the
  `Store` contract and are implemented by the memory, SQLite, and PostgreSQL
  adapters (append-only migrations) and enforced by `NewPolicyStore`. The public
  surface adds `Scheduler`, `SchedulerConfig`, `NewScheduler`, `TickResult`,
  `ScheduleRecord`, `ScheduleExecutionRecord`, `ScheduleFilter`, `CronSchedule`,
  `ParseCronSpec`, `WorkflowResolver`/`WorkflowMap`, and the concurrency and
  execution-status constants; no new module dependency is introduced.
- Pluggable authentication and authorization hooks. A new `Policy` contract lets
  an application allow or deny an agent run, a tool call, a workflow run, or a
  storage operation against a caller `Identity` (subject, tenant, capabilities,
  and attributes) that flows through the run context via `WithIdentity` /
  `IdentityFromContext`. The library ships no identity provider and no concrete
  policy engine — only the hook: `AgentConfig.Policy` and
  `LinearWorkflowConfig.Policy` authorize runs and their tool calls, and
  `NewPolicyStore` wraps any `Store` to authorize every repository read and write
  at method granularity. A denied operation returns a typed, auditable
  `*PolicyDenial` (matched by `errors.Is(err, ErrPolicyDenied)`) that preserves
  the caller, action, resource, and reason; agent and workflow denials are
  recorded in the run result and the terminal run event via the new
  `AgentErrorUnauthorized` / `WorkflowErrorUnauthorized` kinds and the
  `ToolExecutionUnauthorized` tool state. Identity propagates automatically into
  subagent delegations and workflow steps because they share the run context. A
  nil `Policy` allows everything, so the core library stays usable with no policy
  configured, and `AllowAllPolicy` is the explicit no-op. Nothing in the module
  gains a dependency; the contract is standard-library-only.
- Dataset evaluation, scorers, and experiment runs. The new optional `evals`
  package runs a versioned dataset against an agent or workflow, scores every
  case, and persists per-case results so two runs can be compared before a
  release. Nothing in the root module imports it, so an application that does
  not evaluate never compiles it in, and it adds no dependency beyond the lebro
  module and the standard library. `evals.Dataset.Version` is a content hash over
  the ordered, canonicalized cases rather than a caller-supplied label, so "the
  same dataset version" is a verifiable fact: editing, adding, or reordering a
  case changes the version, while reformatting a case's JSON input does not.
  `Target` is what a dataset runs against — `NewAgentTarget` adapts any
  `lebro.Workflow`, which `*lebro.Agent` implements, and `NewWorkflowTarget`
  adapts a JSON-step workflow such as `*lebro.LinearWorkflow`, so one dataset
  serves both kinds without a caller-written adapter; a case carries both an
  `Input` and `Messages`, and a target that cannot invoke a case reports
  `ErrTargetUnsupportedCase` rather than silently reshaping it. Deterministic
  rule scorers ship first and need no provider: `NewExactMatch`, `NewContains`,
  `NewRegexp`, `NewJSONEquals`, and `NewNumericRange`, each validating its
  configuration at construction so an unusable pattern or an inverted range is a
  setup error rather than a per-case surprise. `NewModelScorer` grades behind the
  existing model protocol with a caller-supplied `lebro.Model`, so no provider
  dependency enters the module; it decodes the verdict's score through a pointer
  so a grader that scored 0 stays distinguishable from one that answered
  nothing, and a model failure is reported as a scorer failure rather than a
  zero score, because an unreachable judge must not manufacture a regression. A
  scorer that errors or panics is recorded in `CaseResult.ScorerFailures` and
  leaves the target's own `Status` and `Output` untouched, with
  `ExperimentRecord` counting `TargetFailures` and `ScorerFailures` separately:
  "did the thing under test work?" and "could we measure it?" are different
  findings. A panicking scorer is recovered, so one bad judge cannot abandon the
  run or blind the other scorers on that case. Cases are dispatched across a
  bounded worker pool, but results are ordered by dataset position, so a stored
  record never depends on worker scheduling. `Compare` reports per-scorer and
  per-case deltas and returns `ErrDatasetVersionMismatch` when two records
  disagree on dataset ID or version, since comparing different question sets
  would read as a quality change while really being a change of subject; it names
  the cases whose pass state moved because two offsetting per-case changes cancel
  out in an aggregate mean. Results persist through `evals.Repository`,
  deliberately separate from `lebro.Store` so evaluation data can live in another
  database and an evaluation write can never join the transaction that persists a
  workflow step; `MemoryRepository` ships for tests and single-process use and
  returns caller-owned copies. See `examples/evals-dataset`.
- Typed Go client for the HTTP API. `httpapi.Client` calls a lebro HTTP server
  with the same result and stream contracts the in-process primitives use, so an
  application that moves an agent out of process changes how it constructs the
  call rather than how it reads the answer. It ships in the `httpapi` package
  and decodes the same wire types the server serves, so the client's contract
  cannot drift from the server's; `TestCompatClientCoversEveryRoute` fails if a
  route gains no client method. `NewClient` takes a base URL, an optional
  `*http.Client` for TLS, proxy, and pooling concerns, and a `Header` hook for
  authentication — the package ships no scheme, matching
  `ServerConfig.Middleware` on the serving side. Methods cover every route:
  `Run`, `RunStream`, `RunWorkflow`, `Health`, `ListAgents`, `ListWorkflows`,
  `GetThread`, `ListMessages`, and `OpenAPI`, with `WithThread` binding a run to
  a durable conversation and `WithCursor` and `WithLimit` paging a thread.
  Streamed runs return a `ClientStream` whose `Events`, `Cancel`, `Wait`, and
  `Drain` deliberately mirror `lebro.StreamRun`, so remote and local streaming
  read the same; the terminal event is consumed by the stream and surfaces
  through `Wait` rather than arriving on `Events`, so a caller cannot mistake it
  for another delta or miss it by breaking out of the loop early. `Cancel`
  closes the connection, which the server observes as a disconnect and turns
  into a cancelled run, and it releases the reader goroutine even when the
  caller abandons the stream without draining it. A stream that ends without a
  terminal event reports `ErrStreamIncomplete` rather than an empty success,
  because a dropped connection and a run that failed are different facts.
  Failures arrive as `*APIError` carrying the server's `ErrorCode` and
  unwrapping to the lebro sentinel that classifies it, so
  `errors.Is(err, lebro.ErrAgentToolFailure)` holds for a remote tool failure
  exactly as for a local one. A code maps to a sentinel only where every runtime
  error producing it shares one: `invalid_input` and `invalid_output` are raised
  by both agent and workflow failures, so they carry the code alone rather than
  a sentinel that would be wrong half the time, as do codes with no runtime
  counterpart. A run ended by the caller's context reports the context error it
  observed, so an elapsed deadline matches `context.DeadlineExceeded` rather
  than `context.Canceled`, preserving the distinction the runtime makes locally.
  `ContractVersion` names the wire contract, is published in the document as
  `info.x-lebro-contract-version`, and `Client.CheckCompatibility` compares
  major versions and reports `ErrIncompatibleContract` on a mismatch — called
  explicitly, never on every request, so a run does not pay for a round trip it
  did not ask for. The compatibility suite drives the real server through the
  real client rather than hand-written fixtures. See `examples/http-client`.

- Embeddable HTTP server and generated OpenAPI contract. The new optional
  `httpapi` package serves registered agents and workflows over HTTP and
  publishes the contract as an OpenAPI 3.1 document. It is absent from the core
  dependency graph and imports only the standard library and the `lebro` façade,
  so an application that does not serve HTTP never compiles it in and one that
  does gains no third-party dependency. Only explicitly registered primitives
  are routable: `NewServer` returns a server with nothing exposed, and
  `ExposeAgent` and `ExposeWorkflow` are the only ways to add a route, so a
  primitive that was not registered cannot be reached by guessing an identifier.
  Routes cover run (`POST /agents/{id}/runs`), streaming
  (`POST /agents/{id}/runs/stream`), workflow (`POST /workflows/{id}/runs`),
  thread reads (`GET /threads/{id}` and `GET /threads/{id}/messages`), listings,
  health, and the contract itself at `GET /openapi.json`. Requests supply user
  text only — message roles are fixed server-side, so a client cannot inject a
  system prompt or forge an assistant turn, and thread IDs come from the path or
  query string rather than a request body, so one caller cannot address
  another's durable conversation by naming it in a payload. Failures surface as
  stable `ErrorCode` values derived from the runtime's normalized error kinds,
  each with a fixed public message; internal error text never reaches a response
  body. The streaming route emits Server-Sent Events whose names reuse the
  `RunEventType` vocabulary and always terminates with exactly one terminal
  event, so a client distinguishes a completed run from a dropped connection,
  and closing the connection cancels the run. Streamed deltas pass through a
  `Redactor` first; a nil `Redactor` selects `DefaultRedactor`, which strips
  model-supplied tool-call arguments while passing assistant text and structured
  output through, so a zero-valued configuration streams less rather than more.
  Authentication is deliberately absent — `ServerConfig.Middleware` wraps the
  router and the package stays neutral about the scheme — and workflow resume is
  not exposed, matching the MCP server, because no durable atomic resume claim
  exists yet. The OpenAPI document is generated from the same route table that
  builds the router, so a served route cannot be missing from it; exposed
  primitives are named in their operations' descriptions and each workflow's
  declared input schema is embedded in its request body, keeping the published
  contract as precise as the runtime validation. A request to a route that
  exists but not for the requested method is answered `405` with an `Allow`
  header rather than a `404` implying the resource is absent, and `HEAD` is
  served for every `GET` route so the served surface equals the documented one.
  Non-streaming agent runs do not report token usage: `RunResult` carries no
  aggregate and an agent's `RunListener` is fixed at construction, so a
  per-request total cannot be collected without mutating state shared by
  concurrent requests; configure a `RunListener` on the agent, or use the
  streaming route, whose terminal event carries the run total summed across
  every model call. New public
  types: `httpapi.Server`, `httpapi.ServerConfig`, `httpapi.Redactor`,
  `httpapi.ErrorCode`, and the wire contracts `RunRequest`, `RunResponse`,
  `WorkflowRunRequest`, `WorkflowRunResponse`, `StreamEvent`, `ErrorResponse`,
  `ThreadResponse`, `MessageListResponse`, `HealthResponse`, and the listing
  types. A runnable example is in `examples/http-server`.

- Document ingestion, embeddings, and retrieval tools. Retrieval-augmented
  generation is assembled from provider-neutral contracts — `Document`, `Chunk`,
  `Chunker`, `EmbeddingModel`, and `Retriever` — that persist through the
  existing optional `VectorStore`. `NewIndexer` runs the ingestion pipeline:
  it chunks a document, embeds the chunks in batches, and upserts them with
  their provenance. Chunk IDs are `"<DocumentID>#<Index>"`, so re-ingesting an
  unchanged document replaces its records rather than duplicating them.
  `NewCharacterChunker` is the initial chunking strategy, measuring `Size` and
  `Overlap` in runes so a multi-byte character is never split across chunks.
  `NewVectorRetriever` answers a natural-language query by embedding it and
  searching the index, returning stable source metadata on every hit
  (`DocumentID`, `Source`, and `Index`, recorded under the reserved metadata
  keys `document_id`, `source`, and `chunk_index`); a document whose own
  metadata uses a reserved key is rejected rather than silently overwritten.
  `NewRetrievalTool` exposes a `Retriever` as an ordinary schema-backed `Tool`,
  so an agent selects retrieval through ordinary model tool-calling inside the
  existing bounded loop — no implicit context injection and no hidden agent
  behavior. Retrieval scope is configuration rather than a model's choice: the
  metadata filter is fixed at construction and is not model-settable, a
  model-supplied `top_k` is clamped (falling back from `MaxTopK` to `TopK` to
  `DefaultRetrievalTopK`, so the bound holds even when neither is configured),
  and a caller filter naming an enforced key loses to the configured value.
  Ingestion is lossless: content that is not valid UTF-8 is rejected rather than
  silently rewritten by rune conversion. Agent and workflow packages never
  reference RAG types, so the core runtime remains usable with no RAG or vector
  dependency. New public types: `Document`, `Chunk`, `Chunker`,
  `CharacterChunker`, `CharacterChunkerConfig`, `EmbeddingModel`, `Indexer`,
  `IndexerConfig`, `IndexResult`, `Retriever`, `RetrievalQuery`,
  `RetrievedChunk`, `VectorRetriever`, `VectorRetrieverConfig`, `RetrievalTool`,
  `RetrievalToolConfig`, `RAGError`, `RAGErrorKind`. New error kinds:
  `RAGErrorInvalidDocument`, `RAGErrorChunking`, `RAGErrorEmbedding`,
  `RAGErrorIndexing`, `RAGErrorRetrieval`, with matching
  `ErrRAGInvalidDocument`, `ErrRAGChunking`, `ErrRAGEmbedding`,
  `ErrRAGIndexing`, and `ErrRAGRetrieval` sentinels (all `errors.Is`-compatible,
  with `errors.As` reaching the wrapped `*ModelError` or `ErrVector*` cause). A
  runnable example is in `examples/rag-retrieval`.

- OpenAI-compatible embeddings adapter. `openai.NewEmbedder` implements
  `lebro.EmbeddingModel` against any OpenAI-compatible `/embeddings` endpoint,
  reusing the chat adapter's error classification so one retry policy covers
  both. `Dimension` is required and every response is checked against it, so a
  misconfigured dimension surfaces as an error instead of a corrupt index. The
  adapter reorders the response by each item's declared index rather than
  trusting wire order, and rejects a response whose count, indices, or vector
  widths do not match the request, so a provider that drops or truncates an item
  fails loudly rather than writing misaligned vectors. `RequestDimension` opts
  into sending the `dimensions` parameter for models that support reduction.
  New public types: `openai.Embedder`, `openai.EmbedderConfig`.

- Supervised agent delegation. `NewSubagent` exposes an `Agent` (or any
  `Workflow`) as a named, schema-backed capability that a supervising agent can
  delegate focused work to. A `Subagent` implements `Tool`, so registering one
  in a `ToolRegistry` and listing its ID in the supervisor's definition is
  sufficient: selection happens through ordinary model tool-calling, and the
  delegation reuses the existing execution boundary for input and output schema
  validation, panic containment, and per-agent tool allow-listing. The default
  delegation contract takes a required `task` and optional `context` string and
  returns the child's `agent_id`, `run_id`, `status`, and `output`; both schemas
  are overridable. Delegated runs are bounded independently of the parent:
  `MaxSteps` narrows the child's step budget without mutating the target agent,
  and `Deadline` is layered on the parent context so a child that exhausts its
  own budget fails the delegation while the parent stays live. Thread context is
  isolated by default — a delegated run sees only its task — with `ShareThread`
  and `ShareMetadata` opting into sharing per subagent rather than per call.
  Parent and child runs stay correlated through the run event stream: child
  events carry `ParentRunID`, `ParentStepID`, and `ParentStep` identifying the
  delegating step, while the child run ID is namespaced under the parent run so
  the two are never conflated. Nesting is permitted and bounded per level; there
  is no global depth cap. New public types: `Subagent`, `SubagentConfig`,
  `SubagentError`, `SubagentErrorKind`. New error kinds:
  `SubagentErrorInvalidInput`, `SubagentErrorRunFailed`,
  `SubagentErrorCancelled`, with matching `ErrSubagentInvalidInput`,
  `ErrSubagentRunFailed`, and `ErrSubagentCancelled` sentinels (all
  `errors.Is`-compatible). A runnable example is in
  `examples/supervised-delegation`.

- Provider-neutral vector storage contracts and initial adapters. A new
  `VectorStore` interface provides index management, embedding upsert/delete,
  and cosine-similarity search with metadata filtering and score thresholds.
  The interface is intentionally separate from `Store` so agent and workflow
  packages remain usable with no vector dependency. New types:
  `EmbeddingRecord`, `VectorMetadataFilter`, `SimilarityQuery`,
  `SimilarityResult`. New errors: `ErrVectorNotFound`,
  `ErrVectorAlreadyExists`, `ErrVectorInvalidDimension`,
  `ErrVectorInvalidInput` (all `errors.Is`-compatible). A shared
  `VectorContractSuite` runs adapter-neutral tests covering index lifecycle,
  round-trips, dimension validation, delete semantics, similarity ordering,
  metadata filtering, score thresholds, defensive copies, invalid input, and
  context cancellation. Three adapters ship: `MemoryVectorStore` (brute-force
  cosine, for tests and local development), `SQLiteVectorStore` (vectors as
  JSON TEXT, brute-force in Go, separate schema migrations via
  `vector_schema_migrations` table), and `PostgresVectorStore` (pgvector
  extension, cosine distance via `<=>` operator, separate schema migrations).
  PostgreSQL vector tests are gated by `LEBRO_POSTGRES_TEST_DSN`. New
  dependency: `github.com/pgvector/pgvector-go` (pure Go, no CGO). A runnable
  example is in `examples/vector-search`.

- Bounded parallel fan-out and join for linear workflows. A `StepDefinition`
  may declare a `FanOut` with `Branches` (each with a `Name`, optional
  `InputMapper`, and ordered `Steps`), `MaxParallel` (bounds active branches),
  and `FailurePolicy` (`FanOutFailFast` cancels siblings after the first child
  failure; `FanOutCollectAll` lets every branch finish). The executor runs
  branches concurrently within the configured bound and joins their outputs in
  declaration order as a JSON array of `{"name":"...","output":...}` objects,
  independent of completion timing. `WorkflowRunResult.FanOut` exposes each
  child branch's terminal state and the join outcome; the persisted snapshot
  carries the same records. Fan-out steps must not declare a `Handler`,
  `OutputSchema`, `SuspendSchema`, `Retry`, or conditional `Branches`.
  Suspend within a fan-out child is rejected at runtime as an invalid output.
  Conditional branches inside fan-out children are supported. New error kinds:
  `WorkflowErrorFanOutBranchFailed`, `WorkflowErrorFanOutInputMapperFailed`,
  `WorkflowErrorInvalidFanOutInput` (with matching sentinels). The run emitter
  is now thread-safe so concurrent child step events maintain monotonic
  sequencing. The snapshot envelope version is bumped to `4` and adds the
  optional `fan_out` field; readers tolerate `0`, `1`, `2`, and `3` as legacy.
  New public types: `FanOut`, `FanOutBranch`, `FanOutFailurePolicy`,
  `FanOutInputMapper`, `FanOutBranchResult`, `FanOutJoinResult`.

- Conditional branching for linear workflows. A `StepDefinition` may declare
  `Branches` (each with a `Name`, pure-Go `BranchPredicate`, and ordered
  `Steps`) plus an optional `Default` branch name. When the executor reaches a
  branching step it validates the input against the step's `InputSchema`,
  evaluates each predicate in declared order, selects the first match (or the
  default when no predicate matches), emits a `RunEventBranchSelected`
  ("branch_selected") event carrying the branch name, and runs that branch's
  steps. The selected branch's entry `StepID` is appended to the run `Path`
  (`WorkflowRunResult.Path` and `WorkflowRunRecord.Path`) so the route is
  inspectable and resumable. Branching steps must not declare a `Handler`,
  `OutputSchema`, `SuspendSchema`, or `Retry`; branches must have unique
  non-empty names, at least one step, and non-nil conditions. `Default` must
  name a declared branch. All step IDs (top-level and nested) must be unique
  across the workflow. New error kinds: `WorkflowErrorNoBranchMatched`,
  `WorkflowErrorBranchConditionFailed`, `WorkflowErrorInvalidBranchInput`
  (with matching `ErrWorkflowNoBranchMatched`,
  `ErrWorkflowBranchConditionFailed`, `ErrWorkflowInvalidBranchInput`
  sentinels). The executor uses a frame-stack model so branch steps, nested
  branches, suspend/resume within a branch, and durable persistence all
  coexist. The snapshot envelope version is bumped to `3` and adds the
  optional `path` field; readers tolerate `0`, `1`, and `2` as legacy. SQLite
  and PostgreSQL storage adapters add a `path` column to `workflow_runs`.

- Suspend and resume from durable snapshots. A linear workflow step handler
  can suspend the run by returning a `*SuspendError` wrapping a
  `SuspendSignal` (matched via `errors.Is(err, ErrWorkflowSuspend)`). The
  executor validates the signal's `Contract` against the step's new
  `StepDefinition.SuspendSchema` and persists a suspend snapshot plus a
  `RunStatusSuspended` run record; `WorkflowRunResult.Suspend` carries the
  validated contract and opaque payload. `LinearWorkflow.Resume` loads the
  suspended run and latest suspend snapshot from the bound `Store`, validates
  `WorkflowResumeInput.Input` against the persisted contract, and runs the
  remaining steps without re-executing completed ones. Invalid resume input
  returns `ErrInvalidResumeInput` before any step runs or persistence occurs,
  so the suspended snapshot is not corrupted. Resuming a non-suspended run
  returns `ErrNotSuspended`; resuming without a bound `Store` returns
  `ErrWorkflowResumeRequiresStore`. Two new run events, `RunEventSuspended`
  ("run_suspended") and `RunEventResumed` ("run_resumed"), mark the suspend
  and resume boundaries; both are non-terminal. The snapshot envelope version
  is bumped to `2` and adds the optional `suspend` field; readers tolerate
  `0` and `1` as legacy.

- Durable conversation threads. `AgentConfig.Store` optionally binds an agent
  to a `Store` so that conversation history survives across runs. When set
  and `RunInput.ThreadID` is non-empty, the agent loads prior messages from
  the thread, prepends them to the model request, and appends the new
  transcript on success. Failed runs leave no messages, so the thread's
  message sequence stays valid. When `Store` is nil or `ThreadID` is empty,
  agent behavior is unchanged. The thread is auto-created on the first
  successful run if it does not already exist.
- Thread namespace and ownership fields. `ThreadRecord` carries optional
  `Namespace` and `OwnerID` fields for multi-tenant and embedding
  applications. Both default to empty strings and do not affect repository
  behavior when unset. SQLite and PostgreSQL migrations add the columns
  idempotently; the in-memory adapter handles them via struct copy.
- Initial public contracts for agent-runtime primitives.
- Replaceable JSON Schema Draft 2020-12 validation for tool inputs and outputs.
- Deterministic model fixtures, assertions, streams, and provider contract tests
  for repository tests and examples.
- Provider-neutral text, tool-call, and structured-output model protocol with
  opaque vendor extensions and typed error mapping.
- Schema-backed tool registration and execution with request metadata,
  cancellation, and distinct normalized failure states.
- OpenAI-compatible text-generation model adapter with authenticated HTTP
  requests, timeouts, opaque request extensions, typed error translation, and
  recorded-HTTP contract tests.
- Bounded tool-using agent loop that combines instructions, caller messages, and
  tool schemas into model requests; appends tool calls and tool results to the
  conversation in canonical order; enforces configurable maximum steps and
  deadlines; and returns typed failures for unknown tools, invalid arguments,
  tool failures, provider failures, step-limit exhaustion, and cancellation.
- Schema-constrained structured output for the agent loop. `AgentConfig.OutputSchema`
  and `RunInput.OutputSchema` (per-run override) request a final JSON value that
  conforms to a caller-supplied schema; the agent forwards the schema to the
  model adapter on every step and validates the final assistant payload locally
  via `AgentConfig.SchemaCompiler`. Missing or schema-invalid final output
  returns `AgentErrorInvalidStructuredOutput`. `RunResult.StructuredOutput` and
  `RunResult.DecodeStructuredOutput` expose the validated typed result. Tool use
  and a structured final response compose in a single run.
- Deterministic run record for agent lifecycle events: run start/finish, model
  request start/finish, tool-call requested, tool started/finished, and
  terminal events (succeeded, failed, cancelled). Events carry stable run/step
  IDs, monotonic sequence numbers, timestamps, durations, model usage, and error
  summaries. A RunListener interface and RunRecorder collector capture
  events without requiring an observability backend. Injectable Clock and
  IDSource make event streams reproducible across runs with the same fixture.
- Typed linear workflow execution. `LinearWorkflow` composes ordered, named
  steps (`Step`) whose handlers implement `StepHandler` (or `StepHandlerFunc`
  for ordinary Go functions). Each step declares optional JSON Schemas for its
  input and output; the executor compiles them once and validates every
  handoff: a step's input is validated against its `InputSchema` before the
  handler runs, and the handler's output is validated against its
  `OutputSchema` before passing it to the next step. A failed step stops the
  workflow and the returned `*WorkflowError` identifies the failing step by
  1-indexed position and declared ID. Workflow and step lifecycle records
  (`step_started`, `step_finished`) flow through the existing `RunListener` /
  `RunRecorder` event model, ordered and correlated to one run ID. `WorkflowRunInput`
  and `WorkflowRunResult` carry raw JSON input and output; `DecodeOutput`
  unmarshals the final step result into a caller-supplied value.
- Configurable retry policies for workflow steps. `StepDefinition.Retry`
  optionally binds a `RetryPolicy` (`Attempts`, `Delay`, `Retryable`) to a
  step so transient handler failures are retried instead of failing the whole
  run. `Attempts` is the maximum total attempts (1 = no retry); `Delay` is a
  fixed wait applied before each retry; `Retryable` is a predicate that
  selects which handler errors are retried (`DefaultRetryable` rejects
  context cancellation and deadline errors and accepts all other handler
  errors). Validation errors (invalid step input/output), panics, and context
  cancellation are never retried. Each retry attempt past the first emits
  `step_attempt_started` and `step_attempt_finished` events carrying the
  1-indexed `Attempt` number and the waited `Delay`, between the existing
  `step_started` and `step_finished` events. `WorkflowRunInput.RetryOverrides`
  optionally overrides the per-step policy for a single run, keyed by
  `StepID`: an override with `Attempts == 1` disables retry for that run,
  while an override with `Attempts > 1` enables or changes retry. Invalid
  override values (e.g. `Attempts < 1`) fail the run as a step failure.
- Agent and registered-tool workflow step adapters. `NewAgentStep` turns a
  workflow value into an agent user message and returns structured output or
  final assistant text as JSON; it forwards workflow context, thread ID, and
  metadata. `NewToolStep` invokes a schema-checked `RegisteredTool` with the
  workflow value as arguments and returns validated output. Nested agent run
  events now carry parent workflow run and step correlation fields.
- File-backed SQLite storage. `NewSQLiteStore` opens a pure-Go SQLite database
  (via `modernc.org/sqlite`, no CGO) at a caller-supplied DSN and implements
  the same `Store` and repository contracts as the in-memory adapter:
  threads, messages, workflow runs, and workflow snapshots survive process
  restarts. `Migrate` installs the schema atomically and idempotently; a
  failed migration rolls back and leaves the database in its prior state with
  an actionable error. Concurrent writers serialize on SQLite's transactional
  locking, with lock contention over the busy timeout surfaced as
  `ErrConflict` for callers to retry. A shared storage contract suite in the
  test kit runs against both adapters to keep the memory and durable
  implementations behaviorally aligned.
- PostgreSQL storage. `NewPostgresStore` opens a pure-Go PostgreSQL
  connection pool (via `github.com/jackc/pgx/v5`, no CGO) for production
  deployments where multiple processes share threads and workflow state.
  It implements the same `Store` and repository contracts as the in-memory
  and SQLite adapters: threads, messages, workflow runs, and workflow
  snapshots survive connection pool close/reopen. `Migrate` installs the
  schema atomically and idempotently using a `schema_migrations` version
  table; a failed migration rolls back and leaves the database unchanged
  with an actionable error. Transactions use `READ COMMITTED` isolation;
  serialization failures (SQLSTATE 40001) and lock timeouts (55P03) surface
  as `ErrConflict` for callers to retry, and foreign-key violations (23503)
  map to `ErrNotFound`. Required indexes (`idx_messages_thread_seq`,
  `idx_workflow_snapshots_run_seq`) and migration versioning are documented
  in the README. The adapter passes the shared storage contract suite when
  `LEBRO_POSTGRES_TEST_DSN` is set to a disposable database.
- Durable workflow run snapshots. `LinearWorkflowConfig.Store` optionally
  binds a linear workflow to a `Store` so run state survives at safe step
  boundaries. When set, the executor persists the run record as `Running`
  before the first step, and after every successful step writes a
  `WorkflowSnapshotRecord` plus an updated `WorkflowRunRecord` inside one
  `Store.Transaction` so the boundary is atomic. A persistence failure fails
  the run with `WorkflowErrorStepFailed` wrapping the storage error, so a
  process restart never observes a partially persisted step. `WorkflowRunRecord`
  now carries `CurrentStep`, `CurrentStepID`, `StepOutputs` (ordered completed
  outputs), `Failure` (`*WorkflowFailureData` with kind, step, step ID, and
  message), and `WorkflowVersion` (the opaque definition/version reference from
  `WorkflowDefinition.Version`). `WorkflowSnapshotRecord` carries
  `SchemaVersion` (the executor writes `1`; readers tolerate `0` as legacy).
  A new `ListWorkflowRuns(context.Context, WorkflowRunFilter, PageRequest)`
  method on `WorkflowRunRepository` lists runs for inspection, optionally
  filtered by `WorkflowID` and `Status`. SQLite and PostgreSQL add append-only
  migrations for the new columns; the in-memory adapter handles them via
  struct copy. When `Store` is nil, workflow behavior is unchanged.
- Token and event streaming with cancellation. `StreamingModel` extends the
  provider-neutral `Model` interface with `Stream`, which returns a
  `StreamReader` over ordered `StreamDelta` values (text, tool calls,
  structured output, terminal finish reason, usage). `AsStreamingModel`
  adapts a `Model` into a `StreamingModel` when the concrete value implements
  `Stream`, returning nil otherwise so callers fall back to `Generate`.
  `Agent.RunStream` runs the bounded agent loop against a `StreamingModel`
  and returns a `StreamRun` handle: the caller drains `Deltas` (a
  `<-chan StreamDelta`) in real time, then calls `Wait` (or `Drain`) for the
  terminal `RunResult` and error. The caller must `defer` `StreamRun.Cancel`
  so goroutine and channel resources are released even when the stream is
  abandoned before completion; closing the consumer does not leak goroutines.
  Cancellation propagates through the provider stream, tool execution, and the
  loop itself: cancelling the context (or calling `StreamRun.Cancel`) stops
  active work, closes the delta channel, and returns a result with status
  `RunStatusCancelled` wrapping `ErrAgentCancelled`. A `RunEventDelta`
  (`"model_delta"`) run event is emitted for each delta and flows through the
  existing `RunListener` / `RunRecorder` infrastructure. Streaming and
  non-streaming runs produce equivalent final records: when the configured
  model does not implement `StreamingModel`, `RunStream` falls back to
  `Generate` and emits a single delta per step. The OpenAI-compatible adapter
  implements `StreamingModel` over Server-Sent Events, mapping text deltas,
  usage, finish reasons, and error events into the neutral protocol.

### Changed

- The CI coverage gate is now a ratchet instead of a hard 100% requirement.
  `scripts/check-coverage.sh` fails when total statement coverage drops below
  the recorded baseline in `scripts/coverage-baseline`, suggests moving the
  gate forward when coverage grows, and accepts `--update` to do so; any other
  flag-shaped argument is rejected rather than treated as a profile filename.
  `make check` runs the test suite once through the script instead of twice.

- The release workflow anchors the created release at the merged pull
  request's merge commit rather than the pull_request event's test-merge SHA,
  paginates tag listing before computing the next version instead of reading
  only the first hundred tags, and serializes publishes under a `release`
  concurrency group so two merged PRs cannot race to the same tag. A stray
  `.gitignore` entry that matched any directory named `streaming` was removed.

### Fixed

- OpenAI adapters no longer report a canceled or timed-out request as an HTTP
  status error. When a request is canceled while the error body of a failed
  response is still being read, both the chat and embeddings adapters classified
  the failure from the status code alone, so an abandoned request surfaced as a
  retryable `ModelErrorUnavailable` and `errors.Is(err, context.Canceled)` did
  not hold. The shared classifier now routes a read failure caused by
  cancellation or an elapsed deadline through the transport classifier, matching
  the behavior callers already get when the request itself is interrupted.
