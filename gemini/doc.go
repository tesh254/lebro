// Package gemini provides a native Gemini Developer API adapter.
//
// It supports text, thinking, function calls, streaming, and JSON-schema output. Gemini
// accepts only a documented JSON Schema subset; lebro does not transform or
// weaken caller schemas, so unsupported schemas are returned as provider errors.
// Gemini 2.5 maps ReasoningConfig onto token budgets, while newer Gemini models
// map supported tiers onto thinking levels. Thought text and opaque thought
// signatures are retained for safe multi-turn replay.
package gemini
