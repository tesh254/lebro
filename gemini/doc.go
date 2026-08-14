// Package gemini provides a native Gemini Developer API adapter.
//
// It supports text, function calls, streaming, and JSON-schema output. Gemini
// accepts only a documented JSON Schema subset; lebro does not transform or
// weaken caller schemas, so unsupported schemas are returned as provider errors.
package gemini
