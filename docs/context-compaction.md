# Conversation context compaction

`AgentConfig.ContextCompaction` is opt-in. Applications provide a durable
`ContextSummaryStore`, model context-window metadata, and publication fencing.
Lebro evaluates the final provider-neutral request before every model call,
including calls after tool results.

The default policy triggers at 68% of the context window, targets 50%, reserves
4,096 output tokens and 2,048 tokens for additional tool activity, and limits
the direct no-tool summary request to 1,024 output tokens. Applications should
set tighter values for smaller models.

Preflight estimation is deliberately conservative: UTF-8 serialized bytes plus
protocol overhead are used as a portable upper bound for text. Provider
tokenizers, cached tokens, images, and PDFs do not have one portable exact
accounting method. For durable thread runs, a zero context-window disables
compaction and emits a `context_compaction_skipped` lifecycle event rather
than guessing. Runs without a thread have no durable history to compact.

Summaries are injected as a user-level, explicitly untrusted reference record;
they never become system instructions. Canonical transcript messages are never
deleted. A summary record identifies the exact covered message ID and is
published with compare-and-swap, so concurrent or stale workers cannot replace
a newer summary.

Failures before publication leave the summary head unchanged; a successfully
published revision remains valid even if a later compaction pass fails. Lebro
continues only when the active request still fits the reserved hard budget;
otherwise it returns `ContextCompactionError` with a recoverable failure kind.
Current input or an incomplete tool interaction that cannot be safely compacted
returns `input_too_large`.
