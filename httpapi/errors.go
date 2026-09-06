package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tesh254/lebro"
)

// ErrorCode is the stable, machine-readable classification of a failure. It is
// derived from the runtime's normalized error kinds, so a client can branch on
// a code without parsing prose and without the mapping drifting when an
// internal error message is reworded.
type ErrorCode string

const (
	// ErrorCodeInvalidRequest means the request body was malformed, missing, or
	// structurally invalid before any run started.
	ErrorCodeInvalidRequest ErrorCode = "invalid_request"
	// ErrorCodeInvalidInput means a well-formed request failed schema
	// validation: a workflow input the first step rejected, or tool arguments a
	// tool's input schema rejected.
	ErrorCodeInvalidInput ErrorCode = "invalid_input"
	// ErrorCodeNotFound means the addressed agent, workflow, or thread is not
	// exposed or does not exist.
	ErrorCodeNotFound ErrorCode = "not_found"
	// ErrorCodeInvalidOutput means a tool handler or workflow step produced
	// output that failed its declared output schema, or a model produced
	// structured output that failed the run's output schema.
	ErrorCodeInvalidOutput ErrorCode = "invalid_output"
	// ErrorCodeToolFailure means a tool handler returned an error or panicked.
	ErrorCodeToolFailure ErrorCode = "tool_failure"
	// ErrorCodeStepFailure means a workflow step handler returned an error or
	// panicked.
	ErrorCodeStepFailure ErrorCode = "step_failure"
	// ErrorCodeProviderFailure means the model adapter failed.
	ErrorCodeProviderFailure ErrorCode = "provider_failure"
	// ErrorCodeTimeout means the model provider or stream idle timeout elapsed.
	ErrorCodeTimeout ErrorCode = "timeout"
	// ErrorCodeStepLimitExhausted means the agent loop consumed its step budget
	// without the model producing a terminal response.
	ErrorCodeStepLimitExhausted ErrorCode = "step_limit_exhausted"
	// ErrorCodeCancelled means the run was cancelled, normally because the
	// client closed the connection or a deadline elapsed.
	ErrorCodeCancelled ErrorCode = "cancelled"
	// ErrorCodeMethodNotAllowed means the route exists but not for this method.
	ErrorCodeMethodNotAllowed ErrorCode = "method_not_allowed"
	// ErrorCodeInternal means the failure has no more specific public
	// classification.
	ErrorCodeInternal ErrorCode = "internal_error"
)

// statusClientClosedRequest is the non-standard 499 used to report that the
// client went away mid-run. A cancelled run is neither the server's fault (5xx)
// nor a malformed request (4xx with a body the client can act on), and 499 is
// the widely deployed convention for it.
const statusClientClosedRequest = 499

// publicMessage is the fixed, public wording for each code. Internal error text
// is never surfaced: the mapping is total, so no path can fall through to a raw
// error string.
var publicMessage = map[ErrorCode]string{
	ErrorCodeInvalidRequest:     "the request body is malformed or missing required fields",
	ErrorCodeInvalidInput:       "the request failed schema validation",
	ErrorCodeNotFound:           "the requested resource does not exist",
	ErrorCodeInvalidOutput:      "the run produced output that failed schema validation",
	ErrorCodeToolFailure:        "a tool failed during the run",
	ErrorCodeStepFailure:        "a workflow step failed during the run",
	ErrorCodeProviderFailure:    "the model provider failed",
	ErrorCodeTimeout:            "the model provider timed out",
	ErrorCodeStepLimitExhausted: "the run reached its step limit without completing",
	ErrorCodeCancelled:          "the run was cancelled",
	ErrorCodeMethodNotAllowed:   "the method is not allowed for this route",
	ErrorCodeInternal:           "the server failed to complete the request",
}

// statusForCode maps a public code to its HTTP status. Provider failures are
// reported as 502 rather than 500 because the failure originated upstream of
// this server, which is the distinction an operator's alerting cares about.
var statusForCode = map[ErrorCode]int{
	ErrorCodeInvalidRequest:     http.StatusBadRequest,
	ErrorCodeInvalidInput:       http.StatusBadRequest,
	ErrorCodeNotFound:           http.StatusNotFound,
	ErrorCodeInvalidOutput:      http.StatusBadGateway,
	ErrorCodeToolFailure:        http.StatusInternalServerError,
	ErrorCodeStepFailure:        http.StatusInternalServerError,
	ErrorCodeProviderFailure:    http.StatusBadGateway,
	ErrorCodeTimeout:            http.StatusGatewayTimeout,
	ErrorCodeStepLimitExhausted: http.StatusBadGateway,
	ErrorCodeCancelled:          statusClientClosedRequest,
	ErrorCodeMethodNotAllowed:   http.StatusMethodNotAllowed,
	ErrorCodeInternal:           http.StatusInternalServerError,
}

// classify maps a runtime error to its public code. It matches against the
// exported sentinels rather than concrete types so the mapping keeps working
// through any wrapping the runtime adds, and checks context cancellation first
// because a cancelled run wraps a more specific-looking kind on some paths.
//
// The ordering within each group matters: the more specific sentinel is tested
// before the general one it would otherwise be absorbed by.
func classify(err error) ErrorCode {
	if err == nil {
		return ErrorCodeInternal
	}

	switch {
	case errors.Is(err, lebro.ErrAgentTimeout):
		return ErrorCodeTimeout
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, lebro.ErrAgentCancelled),
		errors.Is(err, lebro.ErrWorkflowCancelled):
		return ErrorCodeCancelled
	}

	switch {
	case errors.Is(err, lebro.ErrAgentUnknownTool),
		errors.Is(err, lebro.ErrToolNotFound),
		errors.Is(err, lebro.ErrNotFound):
		return ErrorCodeNotFound
	case errors.Is(err, lebro.ErrAgentInvalidToolArguments),
		errors.Is(err, lebro.ErrWorkflowInvalidStepInput),
		errors.Is(err, lebro.ErrWorkflowInvalidBranchInput),
		errors.Is(err, lebro.ErrWorkflowInvalidFanOutInput):
		return ErrorCodeInvalidInput
	case errors.Is(err, lebro.ErrAgentInvalidToolOutput),
		errors.Is(err, lebro.ErrAgentInvalidStructuredOutput),
		errors.Is(err, lebro.ErrWorkflowInvalidStepOutput):
		return ErrorCodeInvalidOutput
	case errors.Is(err, lebro.ErrAgentStepLimitExhausted):
		return ErrorCodeStepLimitExhausted
	case errors.Is(err, lebro.ErrAgentToolFailure):
		return ErrorCodeToolFailure
	case errors.Is(err, lebro.ErrAgentProviderFailure):
		return ErrorCodeProviderFailure
	case errors.Is(err, lebro.ErrWorkflowStepFailure),
		errors.Is(err, lebro.ErrWorkflowStepPanicked),
		errors.Is(err, lebro.ErrWorkflowNoBranchMatched),
		errors.Is(err, lebro.ErrWorkflowBranchConditionFailed),
		errors.Is(err, lebro.ErrWorkflowFanOutBranchFailed),
		errors.Is(err, lebro.ErrWorkflowFanOutInputMapperFailed):
		return ErrorCodeStepFailure
	default:
		return ErrorCodeInternal
	}
}

// errorBody renders the public body for a code. An unmapped code cannot occur
// through classify, but a zero value is rendered as an internal error rather
// than as an empty message.
func errorBody(code ErrorCode) ErrorBody {
	message, ok := publicMessage[code]
	if !ok {
		code = ErrorCodeInternal
		message = publicMessage[ErrorCodeInternal]
	}
	return ErrorBody{Code: code, Message: message}
}

// statusFor returns the HTTP status for a code, defaulting to 500 for a code
// with no explicit mapping.
func statusFor(code ErrorCode) int {
	if status, ok := statusForCode[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// writeError writes the public error body for code. A cancelled request is not
// written at all: the client has gone, and writing to a dead connection would
// only produce a spurious error from the ResponseWriter.
func writeError(w http.ResponseWriter, r *http.Request, code ErrorCode) {
	if code == ErrorCodeCancelled && r.Context().Err() != nil {
		return
	}
	writeJSON(w, statusFor(code), ErrorResponse{Error: errorBody(code)})
}

// writeRunError classifies a runtime error and writes its public body.
func writeRunError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, classify(err))
}

// writeJSON encodes value as the response body. The body is marshalled before
// the header is written so an encoding failure can still produce a clean 500
// rather than a truncated 200.
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		body, _ = json.Marshal(ErrorResponse{Error: errorBody(ErrorCodeInternal)})
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
