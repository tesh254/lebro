package obsv_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/obsv"
)

// fixtureStart is the base timestamp for stepping clocks in tests. Real
// timestamps keep durations positive and ordering meaningful.
var fixtureStart = time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC)

// steppingClock advances by a fixed increment on every read, so each lifecycle
// event carries a distinct timestamp and every span gets a non-zero duration.
// A fixed clock would make every span's duration zero, which cannot distinguish
// a correct duration from a dropped one.
type steppingClock struct {
	current time.Time
	step    time.Duration
}

func newSteppingClock(step time.Duration) *steppingClock {
	return &steppingClock{current: fixtureStart, step: step}
}

func (c *steppingClock) Now() time.Time {
	c.current = c.current.Add(c.step)
	return c.current
}

// echoTool returns its input as its output. It is used where a test must prove a
// filter removed a payload: the payload appears on both sides of the call, so a
// leak anywhere is detectable.
type echoTool struct {
	id lebro.ToolID
}

func (t echoTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          t.id,
		Description: "echoes its arguments",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (t echoTool) Execute(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	if len(arguments) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return append(json.RawMessage(nil), arguments...), nil
}

// newRegistry builds a tool registry backed by the real JSON Schema compiler and
// registers the given tools.
func newRegistry(t *testing.T, tools ...lebro.Tool) *lebro.ToolRegistry {
	t.Helper()
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry() error = %v", err)
	}
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	return registry
}

// newObserver constructs an Observer and registers its Close with the test, so a
// forgotten Close cannot leak a goroutine past the test that made it.
func newObserver(t *testing.T, config obsv.Config) *obsv.Observer {
	t.Helper()
	observer, err := obsv.New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := observer.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return observer
}

// synchronousConfig sets a negative queue size so spans export on the emitting
// goroutine. Tests asserting on exported spans then need no polling, and an
// assertion cannot pass merely because a race resolved favorably.
func synchronousConfig(config obsv.Config) obsv.Config {
	config.QueueSize = -1
	return config
}

// waitFor polls condition until it holds or the test times out. It exists for
// the two places a test must observe progress made on another goroutine; every
// other assertion here runs against a synchronous Observer so it needs no
// polling at all.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// spansByKind groups exported spans by kind.
func spansByKind(spans []obsv.Span, kind obsv.SpanKind) []obsv.Span {
	var matched []obsv.Span
	for _, span := range spans {
		if span.Kind == kind {
			matched = append(matched, span)
		}
	}
	return matched
}

// findSpan returns the first span satisfying match.
func findSpan(t *testing.T, spans []obsv.Span, description string, match func(obsv.Span) bool) obsv.Span {
	t.Helper()
	for _, span := range spans {
		if match(span) {
			return span
		}
	}
	t.Fatalf("no exported span matches %s; got %s", description, describeSpans(spans))
	return obsv.Span{}
}

func describeSpans(spans []obsv.Span) string {
	if len(spans) == 0 {
		return "no spans"
	}
	described := make([]string, 0, len(spans))
	for _, span := range spans {
		described = append(described, string(span.Kind)+"/"+span.Name+"/"+string(span.Status))
	}
	return "[" + join(described, " ") + "]"
}

func join(values []string, separator string) string {
	result := ""
	for i, value := range values {
		if i > 0 {
			result += separator
		}
		result += value
	}
	return result
}

// spanByID indexes spans by their SpanID so parentage assertions can walk the
// tree.
func spanByID(spans []obsv.Span) map[obsv.SpanID]obsv.Span {
	indexed := make(map[obsv.SpanID]obsv.Span, len(spans))
	for _, span := range spans {
		indexed[span.SpanID] = span
	}
	return indexed
}

// ancestors walks a span's parent chain, returning the kinds from the span's
// parent up to the root.
func ancestors(t *testing.T, indexed map[obsv.SpanID]obsv.Span, span obsv.Span) []obsv.SpanKind {
	t.Helper()
	var chain []obsv.SpanKind
	current := span
	for depth := 0; current.ParentSpanID != ""; depth++ {
		if depth > 16 {
			t.Fatalf("span %s parent chain exceeds 16 levels; possible cycle", span.SpanID)
		}
		parent, ok := indexed[current.ParentSpanID]
		if !ok {
			t.Fatalf("span %s (%s) references parent %s that was never exported", current.SpanID, current.Kind, current.ParentSpanID)
		}
		chain = append(chain, parent.Kind)
		current = parent
	}
	return chain
}
