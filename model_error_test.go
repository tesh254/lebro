package lebro

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestModelErrorKindsSentinelsAndRetryability(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection reset")
	tests := []struct {
		kind      ModelErrorKind
		sentinel  error
		retryable bool
	}{
		{ModelErrorInvalidRequest, ErrModelInvalidRequest, false},
		{ModelErrorAuthentication, ErrModelAuthentication, false},
		{ModelErrorPermissionDenied, ErrModelPermissionDenied, false},
		{ModelErrorNotFound, ErrModelNotFound, false},
		{ModelErrorRateLimited, ErrModelRateLimited, true},
		{ModelErrorTimeout, ErrModelTimeout, true},
		{ModelErrorUnavailable, ErrModelUnavailable, true},
		{ModelErrorTransport, ErrModelTransport, true},
		{ModelErrorMalformedResponse, ErrModelMalformedResponse, false},
		{ModelErrorUnknown, ErrModelUnknown, false},
		{"vendor_specific", ErrModelUnknown, false},
		{"", ErrModelUnknown, false},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			modelErr := &ModelError{Kind: test.kind, Err: cause}
			if !errors.Is(modelErr, test.sentinel) {
				t.Fatalf("errors.Is(%q, sentinel) = false", test.kind)
			}
			if !errors.Is(modelErr, cause) {
				t.Fatalf("errors.Is(%q, cause) = false", test.kind)
			}
			if got := modelErr.Retryable(); got != test.retryable {
				t.Fatalf("Retryable(%q) = %t, want %t", test.kind, got, test.retryable)
			}
		})
	}
}

func TestModelErrorFormattingAndNilReceiver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  *ModelError
		want string
	}{
		{name: "nil", err: nil, want: "lebro: model failure"},
		{name: "kind", err: &ModelError{Kind: ModelErrorRateLimited}, want: "lebro: model rate_limited"},
		{name: "provider and message", err: &ModelError{Kind: ModelErrorAuthentication, Provider: "openai", Message: "bad key"}, want: "lebro: model authentication from openai: bad key"},
		{name: "cause detail", err: &ModelError{Kind: ModelErrorTransport, Err: errors.New("connection reset")}, want: "lebro: model transport: connection reset"},
		{name: "unknown default", err: &ModelError{}, want: "lebro: model unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.want {
				t.Fatalf("Error() = %q, want %q", got, test.want)
			}
		})
	}

	var nilErr *ModelError
	if nilErr.Unwrap() != nil || nilErr.Is(ErrModelUnknown) || nilErr.Retryable() {
		t.Fatal("nil ModelError reported state")
	}
	modelErr := &ModelError{
		Kind: ModelErrorRateLimited, StatusCode: 429, Code: "rate_limit", RetryAfter: time.Second,
		Extension: []byte(`{"request_id":"req-1"}`),
	}
	if modelErr.Unwrap() != nil || modelErr.Is(ErrModelTimeout) {
		t.Fatal("ModelError matched unrelated state")
	}
	if !strings.Contains(modelErr.Error(), "rate_limited") {
		t.Fatalf("Error() = %q", modelErr.Error())
	}
}
