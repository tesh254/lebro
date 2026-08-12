package httpapi

import "encoding/json"

// Component schema names. Every route table entry references a body schema by
// one of these names, and componentSchemas maps each to its definition, so a
// route cannot reference a schema that does not exist.
const (
	schemaNameString              = "String"
	schemaNameInteger             = "Integer"
	schemaNameRunRequest          = "RunRequest"
	schemaNameRunResponse         = "RunResponse"
	schemaNameWorkflowRunRequest  = "WorkflowRunRequest"
	schemaNameWorkflowRunResponse = "WorkflowRunResponse"
	schemaNameAgentListResponse   = "AgentListResponse"

	schemaNameWorkflowListResponse = "WorkflowListResponse"
	schemaNameThreadResponse       = "ThreadResponse"
	schemaNameMessageListResponse  = "MessageListResponse"
	schemaNameHealthResponse       = "HealthResponse"
	schemaNameErrorResponse        = "ErrorResponse"
	schemaNameStreamEvent          = "StreamEvent"
	schemaNameOpenAPIDocument      = "OpenAPIDocument"
)

// inlineParameterSchemas are the primitive schemas used for path and query
// parameters. They are rendered inline rather than as component references,
// because a "$ref" to a one-line primitive costs a reader a lookup and buys
// nothing.
var inlineParameterSchemas = map[string]json.RawMessage{
	schemaNameString:  json.RawMessage(`{"type":"string"}`),
	schemaNameInteger: json.RawMessage(`{"type":"integer","minimum":0}`),
}

// componentSchemas defines every body schema the document references. They are
// written by hand rather than reflected from the Go structs: this is a
// published contract, and it should change only when someone decides to change
// it, not as a side effect of adding a field.
//
// Each is JSON Schema 2020-12, which is what OpenAPI 3.1 uses.
var componentSchemas = map[string]json.RawMessage{
	schemaNameRunRequest: json.RawMessage(`{
		"type": "object",
		"description": "Input for an agent run. Only user text is accepted; the agent's configured instructions remain the only system message. An omitted or null body runs the agent with no seed messages, which is valid for an agent whose instructions drive the first turn.",
		"properties": {
			"messages": {
				"type": ["array", "null"],
				"description": "Conversation messages seeding the run. Each is treated as a user turn. Null is accepted and equivalent to an empty list.",
				"items": {
					"type": "object",
					"properties": {
						"content": {"type": "string", "description": "The message text. An omitted value is equivalent to an empty string."}
					},
					"additionalProperties": false
				}
			},
			"metadata": {
				"type": ["object", "null"],
				"description": "Caller metadata carried through to tool execution and run events. Null is accepted and equivalent to no metadata.",
				"additionalProperties": {"type": "string"}
			}
		},
		"additionalProperties": false
	}`),

	schemaNameRunResponse: json.RawMessage(`{
		"type": "object",
		"description": "A completed agent run. Token usage is not reported here; configure a RunListener on the agent to observe per-call usage, or read it from the terminal delta on the streaming route.",
		"properties": {
			"run_id": {"type": "string", "description": "Stable identifier for this run."},
			"status": {"type": "string", "description": "Terminal run status.", "enum": ["pending", "running", "succeeded", "failed", "cancelled", "suspended"]},
			"content": {"type": "string", "description": "Text of the terminal assistant message. Empty when the run produced no assistant text."},
			"structured_output": {"description": "Structured payload of the terminal assistant message, present when the agent declares an output schema."}
		},
		"required": ["run_id", "status", "content"],
		"additionalProperties": false
	}`),

	"Usage": json.RawMessage(`{
		"type": "object",
		"description": "Provider-reported token accounting. Values are zero when the provider reports no usage.",
		"properties": {
			"input_tokens": {"type": "integer"},
			"output_tokens": {"type": "integer"},
			"total_tokens": {"type": "integer"}
		},
		"required": ["input_tokens", "output_tokens", "total_tokens"],
		"additionalProperties": false
	}`),

	schemaNameWorkflowRunRequest: json.RawMessage(`{
		"type": "object",
		"description": "Input for a workflow run. The input value is validated against the first step's declared input schema before the run starts.",
		"properties": {
			"input": {"description": "JSON value passed to the first step."},
			"metadata": {
				"type": ["object", "null"],
				"description": "Caller metadata carried through to step execution and run events. Null is accepted and equivalent to no metadata.",
				"additionalProperties": {"type": "string"}
			}
		},
		"additionalProperties": false
	}`),

	schemaNameWorkflowRunResponse: json.RawMessage(`{
		"type": "object",
		"description": "A completed or suspended workflow run.",
		"properties": {
			"run_id": {"type": "string"},
			"status": {"type": "string", "enum": ["pending", "running", "succeeded", "failed", "cancelled", "suspended"]},
			"output": {"description": "Validated output of the final step. Absent when the run suspended or failed."},
			"path": {
				"type": "array",
				"description": "Entry step IDs of the branches selected at each branching step, in order.",
				"items": {"type": "string"}
			},
			"suspend": {"$ref": "#/components/schemas/SuspendResponse"}
		},
		"required": ["run_id", "status"],
		"additionalProperties": false
	}`),

	"SuspendResponse": json.RawMessage(`{
		"type": "object",
		"description": "Details of a workflow run that suspended at a step boundary. Resume is not available over HTTP, so resume_available is always false.",
		"properties": {
			"step": {"type": "integer", "description": "1-indexed position of the suspend boundary."},
			"step_id": {"type": "string"},
			"contract": {"description": "The resume contract published by the suspending step."},
			"resume_available": {"type": "boolean", "const": false}
		},
		"required": ["step", "step_id", "resume_available"],
		"additionalProperties": false
	}`),

	schemaNameAgentListResponse: json.RawMessage(`{
		"type": "object",
		"description": "Every agent exposed by this server, in stable identifier order.",
		"properties": {
			"agents": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string"},
						"name": {"type": "string"},
						"description": {"type": "string"}
					},
					"required": ["id"],
					"additionalProperties": false
				}
			}
		},
		"required": ["agents"],
		"additionalProperties": false
	}`),

	schemaNameWorkflowListResponse: json.RawMessage(`{
		"type": "object",
		"description": "Every workflow exposed by this server, in stable identifier order.",
		"properties": {
			"workflows": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string"},
						"name": {"type": "string"},
						"description": {"type": "string"},
						"version": {"type": "string"},
						"input_schema": {"description": "The first step's declared input schema. Absent when the workflow accepts any input."}
					},
					"required": ["id"],
					"additionalProperties": false
				}
			}
		},
		"required": ["workflows"],
		"additionalProperties": false
	}`),

	schemaNameThreadResponse: json.RawMessage(`{
		"type": "object",
		"description": "A durable conversation's metadata.",
		"properties": {
			"id": {"type": "string"},
			"namespace": {"type": "string"},
			"owner_id": {"type": "string"},
			"metadata": {"description": "Caller-defined thread metadata."},
			"created_at": {"type": "string", "format": "date-time"},
			"updated_at": {"type": "string", "format": "date-time"}
		},
		"required": ["id", "created_at", "updated_at"],
		"additionalProperties": false
	}`),

	schemaNameMessageListResponse: json.RawMessage(`{
		"type": "object",
		"description": "One page of a thread's ordered messages.",
		"properties": {
			"messages": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string"},
						"role": {"type": "string", "enum": ["system", "user", "assistant", "tool"]},
						"content": {"type": "string"},
						"created_at": {"type": "string", "format": "date-time"}
					},
					"required": ["id", "role", "content", "created_at"],
					"additionalProperties": false
				}
			},
			"next_cursor": {"type": "string", "description": "Cursor for the next page. Empty when this is the last page."}
		},
		"required": ["messages"],
		"additionalProperties": false
	}`),

	schemaNameHealthResponse: json.RawMessage(`{
		"type": "object",
		"description": "Server readiness and exposed primitive counts.",
		"properties": {
			"status": {"type": "string", "const": "ok"},
			"agents": {"type": "integer", "description": "Number of exposed agents."},
			"workflows": {"type": "integer", "description": "Number of exposed workflows."}
		},
		"required": ["status", "agents", "workflows"],
		"additionalProperties": false
	}`),

	schemaNameStreamEvent: json.RawMessage(`{
		"type": "object",
		"description": "One Server-Sent Event payload. Delta events carry text, a tool call, or structured output; the single terminal event carries the run status, the run's total token usage, and, on failure, a public error. There is no delta-level error field: an aborting provider stream is reported once through the terminal event.",
		"properties": {
			"type": {"type": "string", "description": "Event name, matching the SSE event field.", "enum": ["model_delta", "run_succeeded", "run_failed", "run_cancelled"]},
			"run_id": {"type": "string", "description": "Present on the terminal event."},
			"text": {"type": "string"},
			"tool_call": {
				"type": "object",
				"description": "A model-requested tool invocation. Arguments are present only when the configured redactor passes them through; the default redactor removes them.",
				"properties": {
					"id": {"type": "string"},
					"tool_id": {"type": "string"},
					"arguments": {"description": "Model-supplied tool arguments."}
				},
				"required": ["id", "tool_id"],
				"additionalProperties": false
			},
			"structured_output": {"description": "Structured payload of the terminal assistant message."},
			"finish_reason": {"type": "string", "enum": ["stop", "length", "tool_calls", "content_filter", "cancelled", "unspecified"]},
			"status": {"type": "string", "description": "Terminal run status. Present on the terminal event.", "enum": ["pending", "running", "succeeded", "failed", "cancelled", "suspended"]},
			"usage": {"$ref": "#/components/schemas/Usage", "description": "Per-call figures on a delta; the run total on the terminal event."},
			"error": {"$ref": "#/components/schemas/Error"}
		},
		"required": ["type"],
		"additionalProperties": false
	}`),

	schemaNameErrorResponse: json.RawMessage(`{
		"type": "object",
		"description": "The body of every non-2xx response.",
		"properties": {
			"error": {"$ref": "#/components/schemas/Error"}
		},
		"required": ["error"],
		"additionalProperties": false
	}`),

	"Error": json.RawMessage(`{
		"type": "object",
		"description": "A public failure description. The message is fixed wording for the code and never carries internal error text.",
		"properties": {
			"code": {
				"type": "string",
				"description": "Stable machine-readable classification.",
				"enum": [
					"invalid_request",
					"invalid_input",
					"not_found",
					"invalid_output",
					"tool_failure",
					"step_failure",
					"provider_failure",
					"step_limit_exhausted",
					"cancelled",
					"method_not_allowed",
					"internal_error"
				]
			},
			"message": {"type": "string"}
		},
		"required": ["code", "message"],
		"additionalProperties": false
	}`),

	schemaNameOpenAPIDocument: json.RawMessage(`{
		"type": "object",
		"description": "An OpenAPI 3.1 document.",
		"properties": {
			"openapi": {"type": "string"}
		},
		"required": ["openapi"]
	}`),
}
