package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ModelErrorKind is the normalized category of a provider failure.
type ModelErrorKind string

const (
	ModelErrorInvalidRequest    ModelErrorKind = "invalid_request"
	ModelErrorAuthentication    ModelErrorKind = "authentication"
	ModelErrorPermissionDenied  ModelErrorKind = "permission_denied"
	ModelErrorNotFound          ModelErrorKind = "not_found"
	ModelErrorRateLimited       ModelErrorKind = "rate_limited"
	ModelErrorTimeout           ModelErrorKind = "timeout"
	ModelErrorUnavailable       ModelErrorKind = "unavailable"
	ModelErrorTransport         ModelErrorKind = "transport"
	ModelErrorMalformedResponse ModelErrorKind = "malformed_response"
	ModelErrorUnknown           ModelErrorKind = "unknown"
)

var (
	ErrModelInvalidRequest    = errors.New("lebro: model invalid request")
	ErrModelAuthentication    = errors.New("lebro: model authentication failed")
	ErrModelPermissionDenied  = errors.New("lebro: model permission denied")
	ErrModelNotFound          = errors.New("lebro: model resource not found")
	ErrModelRateLimited       = errors.New("lebro: model rate limited")
	ErrModelTimeout           = errors.New("lebro: model request timed out")
	ErrModelUnavailable       = errors.New("lebro: model unavailable")
	ErrModelTransport         = errors.New("lebro: model transport failure")
	ErrModelMalformedResponse = errors.New("lebro: malformed model response")
	ErrModelUnknown           = errors.New("lebro: unknown model failure")
)

// ModelError preserves provider failure details behind a normalized category.
// Extension is opaque provider-specific JSON. Err retains the original cause.
type ModelError struct {
	Kind       ModelErrorKind
	Provider   string
	Code       string
	StatusCode int
	RetryAfter time.Duration
	Message    string
	Extension  json.RawMessage
	Err        error
}

func (e *ModelError) Error() string {
	if e == nil {
		return "lebro: model failure"
	}
	kind := e.Kind
	if kind == "" {
		kind = ModelErrorUnknown
	}
	detail := e.Message
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
	provider := ""
	if e.Provider != "" {
		provider = " from " + e.Provider
	}
	if detail == "" {
		return fmt.Sprintf("lebro: model %s%s", kind, provider)
	}
	return fmt.Sprintf("lebro: model %s%s: %s", kind, provider, detail)
}

// Unwrap exposes the original provider or transport error.
func (e *ModelError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is supports errors.Is checks against the normalized ErrModel sentinels while
// Unwrap continues to preserve the original cause.
func (e *ModelError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == modelErrorSentinel(e.Kind)
}

// Retryable reports whether retrying later can reasonably succeed.
func (e *ModelError) Retryable() bool {
	if e == nil {
		return false
	}
	return modelErrorPolicyFor(e.Kind).retryable
}

func modelErrorSentinel(kind ModelErrorKind) error {
	return modelErrorPolicyFor(kind).sentinel
}

type modelErrorPolicy struct {
	sentinel  error
	retryable bool
}

func modelErrorPolicyFor(kind ModelErrorKind) modelErrorPolicy {
	switch kind {
	case ModelErrorInvalidRequest:
		return modelErrorPolicy{sentinel: ErrModelInvalidRequest}
	case ModelErrorAuthentication:
		return modelErrorPolicy{sentinel: ErrModelAuthentication}
	case ModelErrorPermissionDenied:
		return modelErrorPolicy{sentinel: ErrModelPermissionDenied}
	case ModelErrorNotFound:
		return modelErrorPolicy{sentinel: ErrModelNotFound}
	case ModelErrorRateLimited:
		return modelErrorPolicy{sentinel: ErrModelRateLimited, retryable: true}
	case ModelErrorTimeout:
		return modelErrorPolicy{sentinel: ErrModelTimeout, retryable: true}
	case ModelErrorUnavailable:
		return modelErrorPolicy{sentinel: ErrModelUnavailable, retryable: true}
	case ModelErrorTransport:
		return modelErrorPolicy{sentinel: ErrModelTransport, retryable: true}
	case ModelErrorMalformedResponse:
		return modelErrorPolicy{sentinel: ErrModelMalformedResponse}
	default:
		return modelErrorPolicy{sentinel: ErrModelUnknown}
	}
}
