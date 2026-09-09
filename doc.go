// Package lebro provides stable public contracts for building composable AI
// agents and workflows in Go.
//
// The root package is the dependency-light application API. It contains model,
// tool, agent, workflow, persistence, scheduling, authorization, and RAG
// contracts plus their constructors. Optional integrations live in their own
// packages: [github.com/tesh254/lebro/httpapi],
// [github.com/tesh254/lebro/mcp], [github.com/tesh254/lebro/channels],
// [github.com/tesh254/lebro/voice], [github.com/tesh254/lebro/obsv],
// [github.com/tesh254/lebro/evals], and provider adapters. None is imported by
// this package.
//
// Constructors validate configuration up front. Values crossing a model, tool,
// workflow, or storage boundary are schema-checked where a schema is declared;
// callers retain context cancellation and choose policy, provider, and storage
// implementations. See docs/stability.md for compatibility commitments and
// docs/migrations.md before changing persisted or wire-visible deployments.
// Runtime implementation is organized under internal/runtime.
//
// # Message content parts and attachments
//
// A user Message carries multipart input through ContentParts: ordered text,
// image, and document (PDF) parts built with NewTextPart, NewImagePart,
// NewDocumentPart, and NewMessageContentParts. Each adapter maps parts to its
// provider's native input — OpenAI image_url and file blocks, Anthropic base64
// image and document sources, Gemini inlineData blobs — preserving order and
// payloads, and fails with a normalized *ModelError rather than dropping a
// part it cannot represent. Content and ContentParts are mutually exclusive,
// and only user messages may carry parts. Text attachments are ordinary text
// parts wrapped in an XML attachment element (NewTextAttachmentPart) that
// identifies user-provided attachment data without granting it instruction
// priority. Transcripts persist all parts byte-faithfully, and older
// text-only records replay unchanged.
package lebro
