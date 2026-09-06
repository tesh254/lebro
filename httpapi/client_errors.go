package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tesh254/lebro"
)

// Client-side failures that are not a server-reported error code. A response
// the client could not read at all is distinct from a run the server rejected:
// the first says nothing about the request, the second is the server's verdict
// on it.
var (
	// ErrIncompatibleContract reports that the server serves a wire contract
	// whose major version differs from ContractVersion. A client that continued
	// would be decoding a shape it was not built for.
	ErrIncompatibleContract = errors.New("lebro/httpapi: server contract version is incompatible")
	// ErrMalformedResponse reports a response the client could not decode: a
	// body that is not the documented shape, or a stream frame that is not
	// valid SSE. It is a transport or version problem, never a run outcome.
	ErrMalformedResponse = errors.New("lebro/httpapi: malformed response from server")
	// ErrStreamIncomplete reports a stream that ended without a terminal event.
	// The stream contract guarantees exactly one, so its absence means the
	// connection dropped mid-run and the run's outcome is unknown — which is
	// materially different from a run that failed and said so.
	ErrStreamIncomplete = errors.New("lebro/httpapi: stream ended without a terminal event")
)

// sentinelForCode maps a public error code to the root package's error
// sentinel, so a remote failure matches the same errors.Is check as the local
// one it stands for. A caller that handles lebro.ErrAgentToolFailure from
// Agent.Run handles it identically from Client.Run.
//
// A code maps to a sentinel only when every runtime error that produces it
// shares that sentinel. Several codes fail that test: the server derives
// invalid_input from a rejected workflow step input *and* from rejected agent
// tool arguments, and invalid_output likewise covers a bad step output, a bad
// tool output, and a bad structured output. The wire deliberately does not say
// which, so there is no sentinel the client can name without being wrong for
// the other cases — claiming lebro.ErrWorkflowInvalidStepInput for an agent
// run's rejected tool arguments would make errors.Is report a workflow error
// for a run that has no steps.
//
// Those codes therefore map to nil, as do the codes with no runtime counterpart
// at all — a malformed request, a method mismatch, an unclassified internal
// failure. Their APIError still carries the code, which is the honest
// classification and the one a caller should branch on. A sentinel that is
// right only half the time is worse than none: it turns a visible "no match"
// into a silent wrong match.
var sentinelForCode = map[ErrorCode]error{
	// Unambiguous: one runtime failure class each, whatever produced the run.
	ErrorCodeNotFound:           lebro.ErrNotFound,
	ErrorCodeToolFailure:        lebro.ErrAgentToolFailure,
	ErrorCodeStepFailure:        lebro.ErrWorkflowStepFailure,
	ErrorCodeProviderFailure:    lebro.ErrAgentProviderFailure,
	ErrorCodeTimeout:            lebro.ErrAgentTimeout,
	ErrorCodeStepLimitExhausted: lebro.ErrAgentStepLimitExhausted,
	ErrorCodeCancelled:          lebro.ErrAgentCancelled,

	// Ambiguous or unrepresented: branch on Code instead.
	ErrorCodeInvalidInput:     nil,
	ErrorCodeInvalidOutput:    nil,
	ErrorCodeInvalidRequest:   nil,
	ErrorCodeMethodNotAllowed: nil,
	ErrorCodeInternal:         nil,
}

// cancelledAPIError builds the error for a run that ended because the caller's
// context did. The context error is carried rather than folded into the code so
// a deadline stays distinguishable from an explicit cancellation: the runtime
// preserves that distinction locally (see preferContextError in
// internal/runtime), and a caller that retries on context.DeadlineExceeded but
// not on context.Canceled would otherwise mis-branch on a remote run.
func cancelledAPIError(cause error) *APIError {
	return &APIError{
		Code:    ErrorCodeCancelled,
		Message: publicMessage[ErrorCodeCancelled],
		cause:   cause,
	}
}

// APIError is a failure the server reported with a public error code. It is
// returned by every Client method that reaches the server and receives a
// non-2xx response, and by ClientStream.Wait for a run whose terminal event
// carried an error.
//
// Its Unwrap chain reaches the lebro sentinel for the code where one exists, so
// both errors.As(err, &apiErr) and errors.Is(err, lebro.ErrAgentToolFailure)
// work on the same value. A cancelled run additionally unwraps to
// context.Canceled, because that is what a caller who cancelled the context
// will check first.
type APIError struct {
	// Code is the server's stable classification of the failure.
	Code ErrorCode
	// Message is the server's fixed public message for that code.
	Message string
	// StatusCode is the HTTP status the server responded with. It is zero for
	// an error carried on a stream's terminal event, which has no status of its
	// own — the stream itself returned 200 before the run failed.
	StatusCode int

	// cause is the local error that ended the call, when there was one: the
	// caller's context error for a cancelled or timed-out run. It is unexported
	// because it is not part of the wire contract — the server never sends it —
	// but it is surfaced through Unwrap so errors.Is reaches it.
	cause error
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode == 0 {
		return fmt.Sprintf("lebro/httpapi: %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("lebro/httpapi: %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
}

// Unwrap reports the lebro sentinel for this code, plus the context error for a
// run the caller's context ended. It returns a slice so a cancelled run matches
// both lebro.ErrAgentCancelled and the context error a caller would check.
//
// The context error is the specific one observed — context.DeadlineExceeded for
// an elapsed deadline, context.Canceled for an explicit cancel — rather than
// always context.Canceled, so a caller that distinguishes them sees the same
// thing an in-process run reports. A cancelled run the server classified, with
// no local context error to attribute it to, falls back to context.Canceled:
// the run was cancelled, and that is the check a caller writes.
func (e *APIError) Unwrap() []error {
	if e == nil {
		return nil
	}
	unwrapped := make([]error, 0, 2)
	if sentinel := sentinelForCode[e.Code]; sentinel != nil {
		unwrapped = append(unwrapped, sentinel)
	}
	switch {
	case e.cause != nil:
		unwrapped = append(unwrapped, e.cause)
	case e.Code == ErrorCodeCancelled:
		unwrapped = append(unwrapped, context.Canceled)
	}
	if len(unwrapped) == 0 {
		return nil
	}
	return unwrapped
}

// apiErrorFromBody builds an APIError from a decoded error body. A body whose
// code is absent or unrecognized is reported as an internal error rather than
// with an empty code, so a caller always has something to branch on: an
// unparseable classification is a server the client does not understand, which
// is the same practical situation as an unclassified failure.
func apiErrorFromBody(body ErrorBody, status int) *APIError {
	code := body.Code
	message := body.Message
	if _, known := publicMessage[code]; !known {
		// The code is one this build does not know, so the message paired with
		// it is not a message this contract defines either. Carrying it through
		// would let non-canonical — potentially internal — text reach the
		// caller under a code that claims to be the fixed public one. Replace
		// both together.
		code = ErrorCodeInternal
		message = ""
	}
	if message == "" {
		message = publicMessage[code]
	}
	return &APIError{Code: code, Message: message, StatusCode: status}
}

// apiErrorFromStatus builds an APIError for a response whose body could not be
// decoded. The code is inferred from the status so a client behind a proxy that
// replaces error bodies still gets a usable classification rather than a bare
// decode failure.
func apiErrorFromStatus(status int) *APIError {
	code := ErrorCodeInternal
	switch status {
	case http.StatusBadRequest:
		code = ErrorCodeInvalidRequest
	case http.StatusNotFound:
		code = ErrorCodeNotFound
	case http.StatusMethodNotAllowed:
		code = ErrorCodeMethodNotAllowed
	case http.StatusBadGateway:
		code = ErrorCodeProviderFailure
	case statusClientClosedRequest:
		code = ErrorCodeCancelled
	}
	return &APIError{Code: code, Message: publicMessage[code], StatusCode: status}
}
