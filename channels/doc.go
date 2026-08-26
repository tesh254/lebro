// Package channels adapts lebro agents to conversational messaging platforms.
//
// A messaging platform delivers an inbound message to a webhook; the agent runs
// through the ordinary streaming pipeline; the streamed reply is delivered back
// to the same conversation. The package supplies the provider-neutral edges —
// an inbound message contract, deterministic thread mapping, deduplication, and
// a streamed-reply relay — and leaves the platform-specific edges to an Adapter.
//
// # Model
//
// A [Server] routes inbound webhooks to explicitly registered agent-adapter
// pairs. Each pair is reachable at a single route:
//
//	/agents/{id}/channels/{platform}/webhook
//
// where id is the agent's definition ID and platform is the adapter's
// [Adapter.Platform]. Only registered pairs are reachable; an unregistered path
// is not served.
//
// An [Adapter] binds one platform to one agent and implements only the
// platform-specific work: [Adapter.Verify] authenticates a webhook request,
// [Adapter.Decode] parses it into a neutral [InboundMessage], and [Adapter.Send]
// delivers a reply chunk. [NewWebhookAdapter] is a provider-neutral adapter that
// authenticates with an HMAC-SHA256 signature and needs no platform SDK.
//
// # Thread mapping
//
// An inbound message maps deterministically to a durable thread: a [ThreadMapper]
// derives a stable [github.com/tesh254/lebro.ThreadID] from the conversation
// reference, so every message in one platform conversation lands on one persisted
// transcript and a reply continues it. The default [NamespaceThreadMapper] hashes
// the platform, a caller namespace, and the provider conversation key. The
// sender's provider user key becomes the thread owner, mirroring the
// resource/thread split used by messaging runtimes: the owner is the external
// principal, the thread is the conversation.
//
// The sender is also mapped onto a [github.com/tesh254/lebro.Identity] and
// carried on the run context with [github.com/tesh254/lebro.WithIdentity], so a
// configured Policy authorizes the run against the platform user. Inbound content
// is always delivered as a user turn, so a platform payload cannot forge a system
// or assistant turn.
//
// # Authentication
//
// Authentication is per adapter, performed in [Adapter.Verify] before the request
// body is trusted; an unverified request is rejected with 401 and never decoded.
// The generic webhook adapter recomputes an HMAC-SHA256 over the raw body bound to
// a request timestamp and compares it in constant time, and rejects a timestamp
// outside a configurable skew to prevent replay. Adapters requiring provider
// credentials read them from their own configuration; the package holds no
// secrets of its own. The [Server]'s middleware is for cross-cutting concerns
// (logging, rate limiting, tracing), not for request authentication.
// Platforms with a signed handshake response can implement [WebhookResponder];
// the Server verifies the request before writing the platform-specific response.

// # Prompt acknowledgements
//
// Some providers require a prompt webhook acknowledgement. Configure
// [Config.Dispatch] to hand verified, deduplicated work to a durable queue or
// worker before the Server returns HTTP 200. The dispatcher owns reliability
// after accepting the work; leaving it nil keeps synchronous execution and
// reports run or delivery failures as HTTP 500.
//
// # Retries and duplicate delivery
//
// Messaging platforms deliver at least once: a slow acknowledgement, a network
// retry, or a platform-side replay resends the same message. The webhook handler
// chooses status codes so a platform's retry logic is safe — a rejected signature
// is 401, an undecodable body is 400, a handshake or receipt with no message is
// 200, an already-processed delivery is 200, and a run or delivery failure is 500
// so the platform may redeliver.
//
// A [Deduplicator] makes redelivery safe by dropping a message whose provider ID
// has already been processed. The check-and-record is atomic, so two concurrent
// deliveries of one ID cannot both run the agent. The channel handler scopes the
// key by agent and platform, so an equal provider ID from a different route is
// not conflated. [MemoryDeduplicator] retains a bounded in-process window and is
// best-effort beyond it: a redelivery older than the window is no longer
// remembered, so the window should exceed a platform's redelivery horizon.
// [StoreDeduplicator] persists a marker per key through a
// [github.com/tesh254/lebro.Store] so redelivery is recognized however late it
// arrives and across a restart; it is the default whenever a store is configured.
// Its marker set is not self-bounded — markers accumulate with the number of
// distinct messages and are pruned out of band — so it trades storage growth for
// exactness. A message with no provider ID cannot be keyed and is always
// processed.
//
// Reply delivery is not transactional with the run. A run can succeed while a
// later reply chunk fails to deliver; the handler then returns 500 and the
// platform redelivers the original message. Because the redelivery repeats the
// original provider ID, deduplication drops it and the agent does not run again —
// so a reply that failed midway is not automatically re-sent. An adapter that
// needs an at-most-once complete reply should treat the final chunk as the
// authoritative reply and make its delivery idempotent.
package channels
