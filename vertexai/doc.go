// Package vertexai provides a native Google Vertex AI adapter for
// Gemini-hosted models.
//
// The adapter targets the Vertex AI (Gemini Enterprise Agent Platform)
// endpoint and authenticates through Google Application Default Credentials.
// Before the first run:
//
//   - Enable the Vertex AI / Agent Platform API in the target project.
//   - Grant the runtime principal roles/aiplatform.user.
//   - Run `gcloud auth application-default login` locally, or attach a
//     service account when deployed.
//
// It supports text, system instructions, multi-turn messages, tool
// definitions and results, JSON-schema output, streaming, and the same
// reasoning controls as the gemini package, because Vertex serves the same
// Gemini protocol. Vertex response metadata is exposed through
// ModelResponse.Extension under the gemini_response_id and gemini_model keys.
// No API keys are accepted or logged, and errors never include credentials;
// HTTPClient is the one escape hatch — when set, the caller owns
// authentication.
package vertexai
