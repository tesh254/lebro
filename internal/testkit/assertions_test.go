package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

type assertionSpy struct {
	helpers int
	fatal   string
}

func (s *assertionSpy) Helper() { s.helpers++ }

func (s *assertionSpy) Fatalf(format string, args ...any) {
	s.fatal = format
}

func TestAssertionsReportUsefulMismatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(*assertionSpy)
		want string
	}{
		{name: "messages", call: func(spy *assertionSpy) {
			AssertMessages(spy, []lebro.Message{{Role: lebro.RoleUser}}, []lebro.Message{{Role: lebro.RoleAssistant}})
		}, want: "messages mismatch"},
		{name: "events", call: func(spy *assertionSpy) {
			AssertRunEvents(spy, []RunEvent{{ID: "got"}}, []RunEvent{{ID: "want"}})
		}, want: "run events mismatch"},
		{name: "tool calls", call: func(spy *assertionSpy) {
			AssertToolCalls(spy, []ToolCall{{ID: "got"}}, []ToolCall{{ID: "want"}})
		}, want: "tool calls mismatch"},
		{name: "cancellation", call: func(spy *assertionSpy) {
			AssertCancellation(spy, errors.New("different"))
		}, want: "context cancellation"},
		{name: "unexpected error", call: func(spy *assertionSpy) {
			assertNoError(spy, errors.New("boom"))
		}, want: "Generate() error"},
		{name: "missing error", call: func(spy *assertionSpy) {
			assertError(spy, nil)
		}, want: "provider failure"},
		{name: "model error", call: func(spy *assertionSpy) {
			assertModelError(spy, errors.New("different"), lebro.ModelErrorUnavailable)
		}, want: "model error kind"},
		{name: "response", call: func(spy *assertionSpy) {
			assertResponse(spy, lebro.ModelResponse{}, lebro.ModelResponse{FinishReason: lebro.FinishReasonStop})
		}, want: "Generate() response"},
		{name: "missing observation", call: func(spy *assertionSpy) {
			assertObservedRequest(spy, nil, lebro.ModelRequest{})
		}, want: "does not expose"},
		{name: "request observation", call: func(spy *assertionSpy) {
			assertObservedRequest(spy, func() lebro.ModelRequest { return lebro.ModelRequest{Model: "got"} }, lebro.ModelRequest{Model: "want"})
		}, want: "observed request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &assertionSpy{}
			test.call(spy)
			if spy.helpers == 0 || !strings.Contains(spy.fatal, test.want) {
				t.Fatalf("assertion spy = %#v, want %q", spy, test.want)
			}
		})
	}

	spy := &assertionSpy{}
	AssertCancellation(spy, context.Canceled)
	assertNoError(spy, nil)
	assertError(spy, errors.New("expected"))
	assertModelError(spy, &lebro.ModelError{Kind: lebro.ModelErrorUnavailable}, lebro.ModelErrorUnavailable)
	assertResponse(spy, lebro.ModelResponse{}, lebro.ModelResponse{})
	assertObservedRequest(spy, func() lebro.ModelRequest { return lebro.ModelRequest{Model: "same"} }, lebro.ModelRequest{Model: "same"})
	if spy.fatal != "" {
		t.Fatalf("successful assertions failed: %s", spy.fatal)
	}
}
