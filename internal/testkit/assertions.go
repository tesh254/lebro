package testkit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/tesh254/lebro"
)

// TestingT is the subset of testing.TB needed by the reusable assertions.
type TestingT interface {
	Helper()
	Fatalf(string, ...any)
}

// AssertMessages compares provider-neutral messages in order.
func AssertMessages(t TestingT, got, want []lebro.Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// AssertRunEvents compares normalized event content in order. Error identity is
// deliberately reduced to its message so independently constructed adapters
// can share the assertion.
func AssertRunEvents(t TestingT, got, want []RunEvent) {
	t.Helper()
	if !reflect.DeepEqual(normalizeEvents(got), normalizeEvents(want)) {
		t.Fatalf("run events mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// AssertToolCalls compares tool calls and their raw arguments in order.
func AssertToolCalls(t TestingT, got, want []ToolCall) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool calls mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// AssertCancellation verifies that err preserves context cancellation.
func AssertCancellation(t TestingT, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func assertNoError(t TestingT, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
}

func assertError(t TestingT, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Generate() error = nil, want provider failure")
	}
}

func assertModelError(t TestingT, err error, want lebro.ModelErrorKind) {
	t.Helper()
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != want {
		t.Fatalf("Generate() error = %v, want model error kind %q", err, want)
	}
}

func assertResponse(t TestingT, got, want lebro.ModelResponse) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Generate() response = %#v, want %#v", got, want)
	}
}

func assertObservedRequest(t TestingT, observe func() lebro.ModelRequest, want lebro.ModelRequest) {
	t.Helper()
	if observe == nil {
		t.Fatalf("provider harness does not expose its observed request")
		return
	}
	if got := observe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("provider observed request = %#v, want %#v", got, want)
	}
}

type normalizedEvent struct {
	ID        string
	Sequence  int
	At        string
	Type      RunEventType
	CallID    string
	Message   lebro.Message
	ToolCalls []ToolCall
	Err       string
}

func normalizeEvents(events []RunEvent) []normalizedEvent {
	result := make([]normalizedEvent, len(events))
	for i, event := range events {
		result[i] = normalizedEvent{
			ID: event.ID, Sequence: event.Sequence, At: event.At.UTC().Format(time.RFC3339Nano),
			Type: event.Type, CallID: event.CallID, Message: event.Message,
			ToolCalls: cloneToolCalls(event.ToolCalls), Err: errorString(event.Err),
		}
	}
	return result
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
