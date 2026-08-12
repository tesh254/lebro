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
// The mapping is intentionally not total in the reverse direction: several
// runtime sentinels collapse onto one code on the wire, because the code is the
// public classification and the wire deliberately does not distinguish, say, an
// unknown tool from a missing thread. Mapping back therefore picks the sentinel
// that names the class, not one arbitrary member of it.
//
// Codes with no corresponding runtime sentinel — a malformed request, a
// method mismatch, an unclassified internal failure — map to nil. Their
// APIError still carries the code, so a caller branches on that; there is no
// lebro sentinel to claim they are, and inventing one would let
// errors.Is match a local error that cannot occur.
var sentinelForCode = map[ErrorCode]error{
	ErrorCodeInvalidRequest:     nil,
	ErrorCodeInvalidInput:       lebro.ErrWorkflowInvalidStepInput,
	ErrorCodeNotFound:           lebro.ErrNotFound,
	ErrorCodeInvalidOutput:      lebro.ErrWorkflowInvalidStepOutput,
	ErrorCodeToolFailure:        lebro.ErrAgentToolFailure,
	ErrorCodeStepFailure:        lebro.ErrWorkflowStepFailure,
	ErrorCodeProviderFailure:    lebro.ErrAgentProviderFailure,
	ErrorCodeStepLimitExhausted: lebro.ErrAgentStepLimitExhausted,
	ErrorCodeCancelled:          lebro.ErrAgentCancelled,
	ErrorCodeMethodNotAllowed:   nil,
	ErrorCodeInternal:           nil,
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

// Unwrap reports the lebro sentinel for this code, plus context.Canceled for a
// cancelled run. It returns a slice so a cancelled run matches both
// lebro.ErrAgentCancelled and context.Canceled; a caller checking either is
// asking the same question.
func (e *APIError) Unwrap() []error {
	if e == nil {
		return nil
	}
	sentinel := sentinelForCode[e.Code]
	if e.Code == ErrorCodeCancelled {
		if sentinel == nil {
			return []error{context.Canceled}
		}
		return []error{sentinel, context.Canceled}
	}
	if sentinel == nil {
		return nil
	}
	return []error{sentinel}
}

// apiErrorFromBody builds an APIError from a decoded error body. A body whose
// code is absent or unrecognized is reported as an internal error rather than
// with an empty code, so a caller always has something to branch on: an
// unparseable classification is a server the client does not understand, which
// is the same practical situation as an unclassified failure.
func apiErrorFromBody(body ErrorBody, status int) *APIError {
	code := body.Code
	if _, known := publicMessage[code]; !known {
		code = ErrorCodeInternal
	}
	message := body.Message
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
