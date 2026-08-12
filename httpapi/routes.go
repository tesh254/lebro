package httpapi

import "net/http"

// route is one public HTTP operation. The same table builds the mux and the
// OpenAPI document, so a route cannot be served without being documented and a
// documented route cannot be missing from the mux. TestRouteTableMatchesOpenAPI
// asserts that correspondence.
//
// Pattern uses net/http's method-and-wildcard syntax. Path is the same route in
// OpenAPI's "{name}" syntax; the two differ only in that Go permits a trailing
// modifier, so they are stored separately rather than derived by string
// rewriting.
type route struct {
	method      string
	path        string
	operationID string
	summary     string
	description string
	// pathParams names the path wildcards in declaration order.
	pathParams []param
	// queryParams names the accepted query parameters.
	queryParams []param
	// requestBody is the component schema name of the request body, empty for
	// operations that take none.
	requestBody string
	// responseBody is the component schema name of the 200 response body.
	responseBody string
	// streaming marks an operation whose success response is an SSE stream
	// rather than a JSON body.
	streaming bool
	// errorCodes lists the public error codes this operation can produce. The
	// generated document derives its error responses from them, so a code that
	// a handler can return but that is missing here is a documentation gap the
	// route-coverage test can see.
	errorCodes []ErrorCode
}

// param is one documented path or query parameter.
type param struct {
	name        string
	description string
	required    bool
	schema      string
}

// pattern renders the route as a net/http mux pattern.
func (r route) pattern() string { return r.method + " " + r.path }

// routes returns the full public route table. It is a function rather than a
// package variable so each server gets its own copy and a caller cannot mutate
// the table another server is serving.
func routes() []route {
	threadIDParam := param{
		name:        "id",
		description: "Thread identifier.",
		required:    true,
		schema:      schemaNameString,
	}
	agentIDParam := param{
		name:        "id",
		description: "Identifier of an exposed agent.",
		required:    true,
		schema:      schemaNameString,
	}
	workflowIDParam := param{
		name:        "id",
		description: "Identifier of an exposed workflow.",
		required:    true,
		schema:      schemaNameString,
	}
	threadQueryParam := param{
		name:        "thread_id",
		description: "Bind the run to a durable thread. Prior messages in the thread are loaded before the run and the new transcript is appended on success. Requires the server to be configured with a Store.",
		schema:      schemaNameString,
	}

	return []route{
		{
			method:       http.MethodGet,
			path:         "/health",
			operationID:  "getHealth",
			summary:      "Report server readiness",
			description:  "Reports that the server is serving and how many agents and workflows it exposes.",
			responseBody: schemaNameHealthResponse,
		},
		{
			method:       http.MethodGet,
			path:         "/agents",
			operationID:  "listAgents",
			summary:      "List exposed agents",
			description:  "Enumerates every agent registered with ExposeAgent. Agents that were not registered are absent and have no routes.",
			responseBody: schemaNameAgentListResponse,
		},
		{
			method:       http.MethodPost,
			path:         "/agents/{id}/runs",
			operationID:  "createAgentRun",
			summary:      "Run an agent to completion",
			description:  "Executes the bounded agent loop and returns the terminal result. The request supplies user text only; the agent's configured instructions remain the only system message.",
			pathParams:   []param{agentIDParam},
			queryParams:  []param{threadQueryParam},
			requestBody:  schemaNameRunRequest,
			responseBody: schemaNameRunResponse,
			errorCodes: []ErrorCode{
				ErrorCodeInvalidRequest,
				ErrorCodeNotFound,
				ErrorCodeInvalidInput,
				ErrorCodeInvalidOutput,
				ErrorCodeToolFailure,
				ErrorCodeProviderFailure,
				ErrorCodeStepLimitExhausted,
				ErrorCodeCancelled,
				ErrorCodeInternal,
			},
		},
		{
			method:      http.MethodPost,
			path:        "/agents/{id}/runs/stream",
			operationID: "streamAgentRun",
			summary:     "Run an agent and stream its output",
			description: "Executes the bounded agent loop and streams ordered Server-Sent Events. Each event's name is a run event type and its data is a StreamEvent. The stream always ends with exactly one terminal event, so a client can distinguish completion from a dropped connection. Closing the connection cancels the run.",
			pathParams:  []param{agentIDParam},
			queryParams: []param{threadQueryParam},
			requestBody: schemaNameRunRequest,
			streaming:   true,
			errorCodes: []ErrorCode{
				ErrorCodeInvalidRequest,
				ErrorCodeNotFound,
				ErrorCodeInternal,
			},
		},
		{
			method:       http.MethodGet,
			path:         "/workflows",
			operationID:  "listWorkflows",
			summary:      "List exposed workflows",
			description:  "Enumerates every workflow registered with ExposeWorkflow, including the first step's declared input schema when it has one.",
			responseBody: schemaNameWorkflowListResponse,
		},
		{
			method:       http.MethodPost,
			path:         "/workflows/{id}/runs",
			operationID:  "createWorkflowRun",
			summary:      "Run a workflow to completion",
			description:  "Executes the workflow's steps in order and returns the final output. The input is validated against the first step's declared input schema before the run starts. A run that suspends is reported with its resume contract; resume is not available over HTTP.",
			pathParams:   []param{workflowIDParam},
			queryParams:  []param{threadQueryParam},
			requestBody:  schemaNameWorkflowRunRequest,
			responseBody: schemaNameWorkflowRunResponse,
			errorCodes: []ErrorCode{
				ErrorCodeInvalidRequest,
				ErrorCodeNotFound,
				ErrorCodeInvalidInput,
				ErrorCodeInvalidOutput,
				ErrorCodeStepFailure,
				ErrorCodeCancelled,
				ErrorCodeInternal,
			},
		},
		{
			method:       http.MethodGet,
			path:         "/threads/{id}",
			operationID:  "getThread",
			summary:      "Read a thread",
			description:  "Returns a durable conversation's metadata. Requires the server to be configured with a Store.",
			pathParams:   []param{threadIDParam},
			responseBody: schemaNameThreadResponse,
			errorCodes: []ErrorCode{
				ErrorCodeNotFound,
				ErrorCodeInternal,
			},
		},
		{
			method:      http.MethodGet,
			path:        "/threads/{id}/messages",
			operationID: "listThreadMessages",
			summary:     "List a thread's messages",
			description: "Returns one page of a thread's ordered messages. Follow next_cursor until it is empty to read the whole thread.",
			pathParams:  []param{threadIDParam},
			queryParams: []param{
				{
					name:        "cursor",
					description: "Opaque cursor from a previous response's next_cursor.",
					schema:      schemaNameString,
				},
				{
					name:        "limit",
					description: "Maximum messages to return. A zero or absent value lets the storage adapter choose.",
					schema:      schemaNameInteger,
				},
			},
			responseBody: schemaNameMessageListResponse,
			errorCodes: []ErrorCode{
				ErrorCodeInvalidRequest,
				ErrorCodeNotFound,
				ErrorCodeInternal,
			},
		},
		{
			method:       http.MethodGet,
			path:         "/openapi.json",
			operationID:  "getOpenAPI",
			summary:      "Serve the generated OpenAPI document",
			description:  "Returns the OpenAPI 3.1 document describing every route this server serves, generated from the same route table that builds the router.",
			responseBody: schemaNameOpenAPIDocument,
		},
	}
}
