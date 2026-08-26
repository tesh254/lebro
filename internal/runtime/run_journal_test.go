package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// observabilityFixture bundles a store-bound agent with its scripted model so
// tests can run and then inspect durable records.
type observabilityFixture struct {
	store *MemoryStore
	model *scriptedModel
	agent *Agent
}

func newObservabilityAgent(t *testing.T, responses ...scriptedResponse) *observabilityFixture {
	t.Helper()
	model := newScriptedModel(responses...)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Store:      NewMemoryStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &observabilityFixture{
		store: agent.store.(*MemoryStore),
		model: model,
		agent: agent,
	}
}

func (f *observabilityFixture) events(t *testing.T, ctx context.Context, filter RunEventFilter) []RunEventRecord {
	t.Helper()
	page, err := f.store.RunEvents().ListRunEvents(ctx, filter, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return page.Records
}

func TestDurableRunPersistsAttemptsEventsAndTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newObservabilityAgent(t,
		toolCallResponse(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"secret":"args"}`)}),
		textResponse("done"),
	)
	f.model.responses[1].response.Usage = ModelUsage{InputTokens: 12, OutputTokens: 8, ReasoningTokens: 3, TotalTokens: 23}
	inputSchema := json.RawMessage(`{"type":"object"}`)
	outputSchema := json.RawMessage(`{"type":"object"}`)
	tool := toolFunc{
		definition: ToolDefinition{ID: "lookup", InputSchema: inputSchema, OutputSchema: outputSchema},
		execute: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"answer":42}`), nil
		},
	}
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	f.agent.tools = registry
	f.agent.allowed = map[ToolID]struct{}{"lookup": {}}

	result, err := f.agent.Run(ctx, RunInput{
		Messages:    []Message{{Role: RoleUser, Content: "user prompt"}},
		ThreadID:    "thread-obs",
		Annotations: Metadata{"app.tenant": json.RawMessage(`"acme"`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Transcript persisted with annotations.
	page, err := f.store.Messages().ListMessages(ctx, "thread-obs", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 4 { // user + assistant(tool_calls) + tool result + assistant(final)
		t.Fatalf("messages = %d records (%+v), want the caller and loop-produced messages", len(page.Records), page.Records)
	}
	for _, record := range page.Records {
		if record.Annotations["app.tenant"] == nil {
			t.Fatalf("message %q missing run annotations", record.ID)
		}
	}
	finalAssistantID := page.Records[len(page.Records)-1].ID

	// One attempt per model call with usage on the winner only.
	attempts, err := f.store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{RunID: RunID(result.ID)}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 2 {
		t.Fatalf("attempts = %d records, want one per model call", len(attempts.Records))
	}
	winner := attempts.Records[1]
	if winner.Status != ModelAttemptSuccess || winner.FinishReason != FinishReasonStop {
		t.Fatalf("winner attempt = %#v", winner)
	}
	if winner.Usage.TotalTokens != 23 {
		t.Fatalf("winner usage = %+v, want provider-reported totals", winner.Usage)
	}
	firstToolMessage := page.Records[1]
	if firstToolMessage.Message.Role != RoleTool && firstToolMessage.Message.Role != RoleAssistant {
		t.Fatalf("unexpected message role %q", firstToolMessage.Message.Role)
	}
	if len(winner.ProducedMessageIDs) != 1 || winner.ProducedMessageIDs[0] != finalAssistantID {
		t.Fatalf("produced message IDs = %v, want [%s]", winner.ProducedMessageIDs, finalAssistantID)
	}
	if attempts.Records[0].Usage != (ModelUsage{}) {
		t.Fatalf("first attempt usage = %+v, want zero (no response was attached)", attempts.Records[0].Usage)
	}

	// Tool execution records carry lifecycle without arguments or results.
	executions, err := f.store.ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{RunID: RunID(result.ID)}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(executions.Records) != 1 {
		t.Fatalf("tool executions = %d records, want 1", len(executions.Records))
	}
	execution := executions.Records[0]
	if execution.State != ToolExecutionSucceeded || execution.ToolCallID != "call-1" {
		t.Fatalf("tool execution = %#v", execution)
	}
	if execution.StartedAt.IsZero() || execution.FinishedAt.Before(execution.StartedAt) {
		t.Fatalf("tool execution timestamps = %#v", execution)
	}
	if executionsJson, err := json.Marshal(execution); err == nil && strings.Contains(string(executionsJson), "secret") {
		t.Fatalf("tool execution leaked arguments: %s", executionsJson)
	}
	if execution.Metadata["app.tenant"] == nil {
		t.Fatal("tool execution missing run annotations")
	}

	// Events cover model, tool, processor-absent, and terminal states in order.
	events := f.events(t, ctx, RunEventFilter{RunID: RunID(result.ID)})
	if len(events) == 0 {
		t.Fatal("no events persisted")
	}
	types := make([]RunEventType, 0, len(events))
	lastSeq := int64(0)
	for _, event := range events {
		if event.ThreadID != "thread-obs" {
			t.Fatalf("event thread = %q, want thread-obs", event.ThreadID)
		}
		if event.Sequence <= lastSeq {
			t.Fatalf("event sequence %d after %d, want monotonic", event.Sequence, lastSeq)
		}
		lastSeq = event.Sequence
		types = append(types, event.Type)
		if event.Metadata["app.tenant"] == nil {
			t.Fatalf("event %s missing run annotations", event.ID)
		}
	}
	if !types[len(types)-1].IsTerminal() || types[len(types)-1] != RunEventSucceeded {
		t.Fatalf("terminal event = %q, want run_succeeded", types[len(types)-1])
	}
	var sawModelFinished, sawToolFinished bool
	for _, typ := range types {
		switch typ {
		case RunEventModelFinished:
			sawModelFinished = true
		case RunEventToolFinished:
			sawToolFinished = true
		case RunEventDelta:
			t.Fatal("delta events must not be persisted by default")
		}
	}
	if !sawModelFinished || !sawToolFinished {
		t.Fatalf("missing lifecycle events: model_finished=%v tool_finished=%v types=%v", sawModelFinished, sawToolFinished, types)
	}
	// Delta text content never lands in any payload.
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "done") || strings.Contains(string(raw), "user prompt") {
		t.Fatalf("events leaked transcript content: %s", raw)
	}
}

func TestDurableRunFailureRetainsDiagnosticsWithoutTranscript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newObservabilityAgent(t, scriptedResponse{err: &ModelError{Kind: ModelErrorUnavailable, Provider: "fixture", Message: "offline"}})
	result, err := f.agent.Run(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}, ThreadID: "thread-fail"})
	if err == nil {
		t.Fatal("Run() error = nil, want failure")
	}

	attempts, err := f.store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 {
		t.Fatalf("failed-run attempts = %d records, want 1", len(attempts.Records))
	}
	attempt := attempts.Records[0]
	if attempt.Status != ModelAttemptFailed || attempt.ErrorKind != string(ModelErrorUnavailable) {
		t.Fatalf("failure attempt = %#v", attempt)
	}

	events := f.events(t, ctx, RunEventFilter{RunID: RunID(result.ID)})
	if len(events) == 0 {
		t.Fatal("failed run retained no diagnostic events")
	}
	terminal := events[len(events)-1]
	if terminal.Type != RunEventFailed || terminal.ErrorKind != string(ModelErrorUnavailable) {
		t.Fatalf("terminal event = %#v, want run_failed classified as the model error kind", terminal)
	}

	if _, err := f.store.Threads().GetThread(ctx, "thread-fail"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed run claimed a transcript: get thread error = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Messages().ListMessages(ctx, "thread-fail", PageRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed run wrote messages: %v", err)
	}
}

func TestDurableRunCancellationRetainsRecordsDespiteDeadContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	f := newObservabilityAgent(t, scriptedResponse{waitForCancel: true})
	resultCh := make(chan struct{})
	var runResult RunResult
	go func() {
		defer close(resultCh)
		runResult, _ = f.agent.Run(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "hello"}}, ThreadID: "thread-cancel"})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-resultCh

	if runResult.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", runResult.Status)
	}
	// The run context is dead; verification needs a live one. This also
	// proves the diagnostic flush used a detached context, not the cancelled
	// run's.
	verifyCtx := context.Background()
	attempts, err := f.store.ModelAttempts().ListModelAttempts(verifyCtx, ModelAttemptFilter{}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 || attempts.Records[0].Status != ModelAttemptCancelled {
		t.Fatalf("cancelled-run attempts = %#v, want one cancelled attempt", attempts.Records)
	}
	foundCancelled := false
	for _, event := range f.events(t, verifyCtx, RunEventFilter{}) {
		if event.Type == RunEventCancelled {
			foundCancelled = true
			if event.ErrorKind != string(AgentErrorCancelled) {
				t.Fatalf("cancelled event classification = %q", event.ErrorKind)
			}
		}
	}
	if !foundCancelled {
		t.Fatal("run_cancelled event was not retained")
	}
	if _, err := f.store.Messages().ListMessages(verifyCtx, "thread-cancel", PageRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled run wrote messages: %v", err)
	}
}

func TestDurableRunAlreadyCancelledContextPersistsDiagnostic(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newObservabilityAgent(t, textResponse("unused"))
	result, err := f.agent.Run(ctx, RunInput{ThreadID: "thread-pre-cancel"})
	if err == nil || result.Status != RunStatusCancelled {
		t.Fatalf("Run = %#v, %v; want cancelled result", result, err)
	}
	events := f.events(t, context.Background(), RunEventFilter{RunID: RunID(result.ID)})
	if len(events) != 1 || events[0].Type != RunEventCancelled {
		t.Fatalf("pre-cancelled events = %#v, want one durable cancellation", events)
	}
}

func TestDurableDirectAttemptUsesStableProviderIdentity(t *testing.T) {
	t.Parallel()
	f := newObservabilityAgent(t, textResponse("ok"))
	result, err := f.agent.Run(context.Background(), RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := f.store.ModelAttempts().ListModelAttempts(context.Background(), ModelAttemptFilter{RunID: RunID(result.ID)}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 || attempts.Records[0].Provider != "direct" {
		t.Fatalf("direct attempt = %#v, want provider=direct", attempts.Records)
	}
}

func TestDurableRunStreamingPathPersistsRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stream := newStreamScriptedModel(
		[]StreamDelta{{Text: "he"}, {Text: "llo", FinishReason: FinishReasonStop, Usage: ModelUsage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7}}},
	)
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      stream,
		Store:      NewMemoryStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := agent.RunStream(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}, ThreadID: "thread-stream"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Drain()
	if err != nil {
		t.Fatal(err)
	}

	store := agent.store.(*MemoryStore)
	attempts, err := store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{RunID: RunID(result.ID)}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 1 || attempts.Records[0].Usage.TotalTokens != 7 {
		t.Fatalf("streaming attempts = %#v, want one attempt carrying stream usage", attempts.Records)
	}
	events, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{RunID: RunID(result.ID)}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Records {
		if event.Type == RunEventDelta {
			t.Fatal("streaming deltas must not be persisted")
		}
	}
	if messages, err := store.Messages().ListMessages(ctx, "thread-stream", PageRequest{}); err != nil || len(messages.Records) != 2 {
		t.Fatalf("streaming transcript = %#v err=%v, want user+assistant", messages, err)
	}
}

func TestDurableRunRejectsInvalidAnnotations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newObservabilityAgent(t, textResponse("ok"))
	_, err := f.agent.Run(ctx, RunInput{
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		Annotations: Metadata{"notnamespaced": json.RawMessage(`"x"`)},
	})
	if err == nil {
		t.Fatal("Run() accepted non-namespaced annotations")
	}
	if !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("error = %v, want an annotations validation failure", err)
	}
}

func TestStoreWithoutObservabilityOptInKeepsRunsWorking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := NewMemoryStore()
	wrapped := &opaqueStoreWrapper{Store: base}
	model := newScriptedModel(textResponse("ok"))
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Model:      model,
		Store:      wrapped,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}, ThreadID: "thread-plain"})
	if err != nil {
		t.Fatalf("Run() = %v; stores that skip observability support must not break runs", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q", result.Status)
	}
	if page, err := base.Messages().ListMessages(ctx, "thread-plain", PageRequest{}); err != nil || len(page.Records) != 2 {
		t.Fatalf("transcript = %#v err=%v, want persisted normally", page, err)
	}
}

// opaqueStoreWrapper hides the observability capability from type assertions.
type opaqueStoreWrapper struct{ Store }

func (w *opaqueStoreWrapper) RepositoriesForTest() Repositories { return w.Store }

var (
	_ Store                     = (*opaqueStoreWrapper)(nil)
	_ ObservabilityRepositories = (*MemoryStore)(nil)
)

func TestRouterFallbackLineageIsFullyPersisted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	primaryErr := &ModelError{Kind: ModelErrorUnavailable, Provider: "primary", Message: "down"}
	primary := newScriptedModel(scriptedResponse{err: primaryErr})
	fallback := newScriptedModel(func() scriptedResponse {
		r := textResponse("recovered")
		r.response.Usage = ModelUsage{InputTokens: 9, OutputTokens: 4, TotalTokens: 13}
		return r
	}())
	registry := NewProviderRegistry()
	if err := registry.Register(ProviderEntry{ID: "primary", Model: primary}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(ProviderEntry{ID: "secondary", Model: fallback}); err != nil {
		t.Fatal(err)
	}
	router, err := NewModelRouter(ModelRouterConfig{
		Registry: registry,
		Fallback: &FallbackPolicy{Chain: []ProviderID{"secondary"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "agent", Model: "fixture-model"},
		Router:     router,
		Store:      NewMemoryStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(ctx, RunInput{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}

	store := agent.store.(*MemoryStore)
	attempts, err := store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{RunID: RunID(result.ID)}, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Records) != 2 {
		t.Fatalf("fallback lineage = %d records, want every attempt", len(attempts.Records))
	}
	if attempts.Records[0].Provider != "primary" || attempts.Records[0].Status != ModelAttemptFallback {
		t.Fatalf("primary attempt = %#v, want fallback status per routing contract", attempts.Records[0])
	}
	if attempts.Records[0].ErrorMessage != "redacted" {
		t.Fatalf("primary attempt must redact the provider message: %#v", attempts.Records[0])
	}
	if attempts.Records[1].Provider != "secondary" || attempts.Records[1].Status != ModelAttemptSuccess {
		t.Fatalf("secondary attempt = %#v, want the successful winner", attempts.Records[1])
	}
	if attempts.Records[1].Usage.TotalTokens != 13 {
		t.Fatalf("winner usage = %+v, want the recovered call's tokens", attempts.Records[1].Usage)
	}
}

func TestMetadataValidationBounds(t *testing.T) {
	t.Parallel()

	if err := Metadata(nil).Validate(); err != nil {
		t.Fatalf("nil metadata invalid: %v", err)
	}
	valid := Metadata{"app.id": json.RawMessage(`"x"`), "plugin.compaction": json.RawMessage(`{"policy":"rolling"}`)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(Metadata)
	}{
		{"reserved namespace", func(m Metadata) { m["lebro.internal"] = json.RawMessage(`"x"`) }},
		{"missing namespace", func(m Metadata) { m["customer"] = json.RawMessage(`"x"`) }},
		{"empty namespace", func(m Metadata) { m[".customer"] = json.RawMessage(`"x"`) }},
		{"empty key suffix", func(m Metadata) { m["app."] = json.RawMessage(`"x"`) }},
		{"bad characters", func(m Metadata) { m["app cu!stomer"] = json.RawMessage(`"x"`) }},
		{"empty value", func(m Metadata) { m["app.x"] = json.RawMessage(``) }},
		{"invalid JSON value", func(m Metadata) { m["app.x"] = json.RawMessage(`{"a":`) }},
		{"too deep", func(m Metadata) {
			m["app.deep"] = json.RawMessage(strings.Repeat(`{"a":`, MaxMetadataDepth+1) + `"leaf"` + strings.Repeat(`}`, MaxMetadataDepth+1))
		}},
	}
	for _, test := range tests {
		value := Metadata{}
		test.mutate(value)
		if err := value.Validate(); err == nil {
			t.Fatalf("%s: validation passed, want rejection", test.name)
		}
	}

	tooMany := Metadata{}
	for i := 0; i <= MaxMetadataEntries; i++ {
		tooMany[fmt.Sprintf("app.key%02d", i)] = json.RawMessage(`"v"`)
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("entry-count bound not enforced")
	}

	big := Metadata{"app.big": json.RawMessage(`"` + strings.Repeat("x", MaxMetadataBytes) + `"`)}
	if err := big.Validate(); err == nil {
		t.Fatal("size bound not enforced")
	}

	shallow := Metadata{"app.shallow": json.RawMessage(strings.Repeat(`{"a":`, MaxMetadataDepth-1) + `"leaf"` + strings.Repeat(`}`, MaxMetadataDepth-1))}
	if err := shallow.Validate(); err != nil {
		t.Fatalf("depth-%d value rejected: %v", MaxMetadataDepth-1, err)
	}
}

func TestJournalConcurrentCaptureIsRaceFree(t *testing.T) {
	t.Parallel()
	journal := newRunJournal(NewFixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), NewMemoryStore(), "run-race", "", ObservabilityScope{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			journal.OnRunEvent(RunEvent{Sequence: 1, Type: RunEventStarted, Timestamp: time.Now()})
			journal.beginModelAttempt("", "")
			journal.completeModelAttempt(ModelAttempt{Status: ModelAttemptSuccess})
			journal.finishModelCall(ModelUsage{}, FinishReasonStop, nil)
			journal.toolStarted(1, "step", ModelToolCall{ID: "c", ToolID: "t"})
			journal.toolFinished(ToolExecutionResult{ToolID: "t", State: ToolExecutionSucceeded})
			journal.linkProducedMessages([]string{"m"})
		}()
	}
	wg.Wait()
	events, attempts, tools := journal.snapshot()
	if len(events) != 16 || len(tools) < 16 {
		t.Fatalf("concurrent capture lost records: %d events, %d attempts, %d tools", len(events), len(attempts), len(tools))
	}
}

func TestRunEventPayloadAllowlist(t *testing.T) {
	t.Parallel()

	stepAttempt := runEventPayload(RunEvent{Type: RunEventStepAttemptStarted, Attempt: 2, Delay: 1500 * time.Millisecond})
	if string(stepAttempt) != `{"attempt":2,"delay_ns":1500000000}` {
		t.Fatalf("step attempt payload = %s", stepAttempt)
	}
	loop := runEventPayload(RunEvent{Type: RunEventLoopIterationFinished, Iteration: 3})
	if string(loop) != `{"iteration":3}` {
		t.Fatalf("loop payload = %s", loop)
	}
	route := runEventPayload(RunEvent{Type: RunEventRouteSelected, DeltaText: `["a","b"]`})
	if string(route) != `["a","b"]` {
		t.Fatalf("route payload = %s", route)
	}
	if got := runEventPayload(RunEvent{Type: RunEventRouteSelected, DeltaText: "not json"}); got != nil {
		t.Fatalf("invalid route payload = %s, want nil", got)
	}
	for _, typ := range []RunEventType{RunEventStarted, RunEventModelFinished, RunEventToolFinished, RunEventSucceeded} {
		if got := runEventPayload(RunEvent{Type: typ}); got != nil {
			t.Fatalf("payload for %s = %s, want nil", typ, got)
		}
	}
}

func TestClassifyRunError(t *testing.T) {
	t.Parallel()

	if kind, message := classifyRunError(nil); kind != "" || message != "" {
		t.Fatalf("nil error classified as %q/%q", kind, message)
	}
	kind, _ := classifyRunError(&AgentError{Kind: AgentErrorUnknownTool})
	if kind != string(AgentErrorUnknownTool) {
		t.Fatalf("agent error kind = %q", kind)
	}
	kind, _ = classifyRunError(context.Canceled)
	if kind != string(AgentErrorCancelled) {
		t.Fatalf("canceled kind = %q", kind)
	}
	kind, _ = classifyRunError(context.DeadlineExceeded)
	if kind != "deadline_exceeded" {
		t.Fatalf("deadline kind = %q", kind)
	}
	kind, _ = classifyRunError(errors.New("boom"))
	if kind != "error" || !strings.Contains(kind, "") {
		t.Fatalf("plain error kind = %q", kind)
	}
}

func TestMergeMetadataOverlay(t *testing.T) {
	t.Parallel()

	base := Metadata{"app.a": json.RawMessage(`1`), "app.b": json.RawMessage(`2`)}
	overlay := Metadata{"app.b": json.RawMessage(`3`)}
	merged := mergeMetadata(base, overlay)
	if len(merged) != 2 || string(merged["app.a"]) != "1" || string(merged["app.b"]) != "3" {
		t.Fatalf("merged = %#v", merged)
	}
	// Mutating inputs after merging must not affect the result.
	base["app.a"] = json.RawMessage(`99`)
	if string(merged["app.a"]) != "1" {
		t.Fatalf("merge aliases its input")
	}
}

func TestObservabilityRecordValidators(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	valid := ModelAttemptRecord{ID: "a", RunID: "r", Index: 1, Status: ModelAttemptSuccess,
		FinishReason: FinishReasonStop, StartedAt: now, FinishedAt: now.Add(time.Second),
		Usage:    ModelUsage{InputTokens: -1},
		Metadata: Metadata{},
	}
	err := validateModelAttemptRecord(valid)
	var validationErr error
	if err == nil {
		t.Fatal("negative usage accepted")
	}
	_ = validationErr

	badReason := valid
	badReason.Usage = ModelUsage{}
	badReason.FinishReason = "nonsense"
	if validateModelAttemptRecord(badReason) == nil {
		t.Fatal("invalid finish reason accepted")
	}
	badMessages := badReason
	badMessages.FinishReason = FinishReasonStop
	badMessages.ProducedMessageIDs = []string{""}
	if validateModelAttemptRecord(badMessages) == nil {
		t.Fatal("empty produced message ID accepted")
	}
	badWindow := badMessages
	badWindow.ProducedMessageIDs = nil
	badWindow.FinishedAt = now.Add(-time.Second)
	if validateModelAttemptRecord(badWindow) == nil {
		t.Fatal("inverted time window accepted")
	}

	tool := ToolExecutionRecord{ID: "t", RunID: "r", ToolCallID: "c", ToolID: "tool",
		State: ToolExecutionHandlerError, StartedAt: now, FinishedAt: now.Add(10 * time.Millisecond),
		Metadata: Metadata{"app.x": json.RawMessage(`"y"`)}}
	if validateToolExecutionRecord(tool) != nil {
		t.Fatal("valid tool record rejected")
	}
	noCall := tool
	noCall.ToolCallID = ""
	if validateToolExecutionRecord(noCall) == nil {
		t.Fatal("missing tool call ID accepted")
	}
	noStart := tool
	noStart.StartedAt = time.Time{}
	if validateToolExecutionRecord(noStart) == nil {
		t.Fatal("missing start accepted")
	}
	inverted := tool
	inverted.FinishedAt = tool.StartedAt.Add(-time.Millisecond)
	if validateToolExecutionRecord(inverted) == nil {
		t.Fatal("tool record finishing before starting accepted")
	}
	badMetaTool := tool
	badMetaTool.Metadata = Metadata{"nope": json.RawMessage(`1`)}
	if validateToolExecutionRecord(badMetaTool) == nil {
		t.Fatal("tool record with invalid metadata accepted")
	}
	attemptWithBadMeta := ModelAttemptRecord{ID: "a", RunID: "r", Index: 1, Status: ModelAttemptSuccess,
		StartedAt: now, FinishedAt: now, Metadata: Metadata{"lebro.x": json.RawMessage(`1`)}}
	if validateModelAttemptRecord(attemptWithBadMeta) == nil {
		t.Fatal("attempt with reserved-namespace metadata accepted")
	}

	event := RunEventRecord{ID: "e", RunID: "r", Sequence: 4, Type: RunEventStarted, Timestamp: now,
		Payload: json.RawMessage(`{"ok":true}`), Plugin: &PluginAttribution{ID: "p"}}
	if validateRunEventRecord(event) != nil {
		t.Fatal("valid event rejected")
	}
	badSeq := event
	badSeq.Sequence = 0
	if validateRunEventRecord(badSeq) == nil {
		t.Fatal("zero sequence accepted")
	}
	badType := event
	badType.Type = ""
	if validateRunEventRecord(badType) == nil {
		t.Fatal("missing type accepted")
	}
	badPlugin := event
	badPlugin.Plugin = &PluginAttribution{}
	if validateRunEventRecord(badPlugin) == nil {
		t.Fatal("plugin attribution without ID accepted")
	}
	badPayload := event
	badPayload.Payload = json.RawMessage(`nope`)
	if validateRunEventRecord(badPayload) == nil {
		t.Fatal("invalid payload JSON accepted")
	}
}

func TestSharedSQLEncodingHelpers(t *testing.T) {
	t.Parallel()

	extrasPayload, extrasPluginID, _, _, _ := obsEventExtras(RunEventRecord{})
	if extrasPayload != nil || extrasPluginID != "" {
		t.Fatalf("empty extras = %#v %q", extrasPayload, extrasPluginID)
	}
	pluginPayload, pluginID, pluginVersion, _, _ := obsEventExtras(RunEventRecord{Payload: json.RawMessage(`{}`), Plugin: &PluginAttribution{ID: "p", Version: "v"}})
	if pluginPayload != "{}" || pluginID != "p" || pluginVersion != "v" {
		t.Fatalf("plugin extras = %#v %q %q", pluginPayload, pluginID, pluginVersion)
	}

	if got := nullableJSON(json.RawMessage("null")); got != nil {
		t.Fatalf("null JSON column value = %v", got)
	}
	if got := obsMetadataJSON(nil); got != nil {
		t.Fatalf("nil metadata column value = %v", got)
	}
	metadata, err := obsParseMetadata(sql.NullString{})
	if metadata != nil || err != nil {
		t.Fatalf("null metadata parse = %#v %v", metadata, err)
	}
	if _, err := obsParseMetadata(sql.NullString{String: "{", Valid: true}); err == nil {
		t.Fatal("corrupt metadata accepted")
	}
	values := obsStringArray([]string{"a"})
	if values == nil {
		t.Fatalf("string array = %v", values)
	}
}

func TestSQLiteObservabilityRepositoryGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := NewSQLiteStore(t.TempDir() + "/guards.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	badPage := PageRequest{Cursor: "not-a-number"}
	if _, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{}, badPage); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("ListRunEvents cursor = %v", err)
	}
	if _, err := store.ModelAttempts().ListModelAttempts(ctx, ModelAttemptFilter{}, badPage); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("ListModelAttempts cursor = %v", err)
	}
	if _, err := store.ToolExecutions().ListToolExecutions(ctx, ToolExecutionFilter{}, badPage); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("ListToolExecutions cursor = %v", err)
	}

	// Event provider and tool filters narrow like MemoryStore.
	mustAppendEvent := func(record RunEventRecord) {
		t.Helper()
		if err := store.RunEvents().AppendRunEvents(ctx, []RunEventRecord{record}); err != nil {
			t.Fatal(err)
		}
	}
	filterNow := time.Now().UTC()
	mustAppendEvent(RunEventRecord{ID: "e2", RunID: "r2", Sequence: 1, Type: RunEventFailed,
		Timestamp: filterNow, Provider: "openai", ToolID: "lookup"})
	if page, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{Provider: "anthropic"}, PageRequest{}); err != nil || len(page.Records) != 0 {
		t.Fatalf("provider filter = %#v err=%v", page, err)
	}
	if page, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{ToolID: "clock"}, PageRequest{}); err != nil || len(page.Records) != 0 {
		t.Fatalf("tool filter = %#v err=%v", page, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.RunEvents().AppendRunEvents(canceled, nil); err == nil {
		t.Fatal("canceled context accepted")
	}
	if err := store.ModelAttempts().SaveModelAttempts(canceled, nil); err == nil {
		t.Fatal("canceled context accepted")
	}
	if err := store.ToolExecutions().SaveToolExecutions(canceled, nil); err == nil {
		t.Fatal("canceled context accepted")
	}

	now := time.Now().UTC()
	if err := store.RunEvents().AppendRunEvents(ctx, []RunEventRecord{{ID: "e1", RunID: "r1", Sequence: 1,
		Type: RunEventStarted, Timestamp: now, Metadata: Metadata{"app.x": json.RawMessage(`1`)}}}); err != nil {
		t.Fatal(err)
	}
	page, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{From: now.Add(-time.Second), To: now.Add(time.Second)}, PageRequest{})
	if err != nil || len(page.Records) < 1 {
		t.Fatalf("time-filtered list = %#v err=%v", page, err)
	}
	if page.Records[0].Metadata["app.x"] == nil {
		t.Fatal("metadata lost in sqlite round-trip")
	}

	// Transaction-scoped accessors expose the same repositories.
	err = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		txObs, ok := repos.(ObservabilityRepositories)
		if !ok {
			t.Fatal("sqlite transaction repositories lost observability support")
		}
		if txObs.RunEvents() == nil || txObs.ModelAttempts() == nil || txObs.ToolExecutions() == nil {
			t.Fatal("transaction observability accessors returned nil repositories")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Every filter field narrows the SQL adapters like MemoryStore.
	mustSave := func(attempt ModelAttemptRecord, execution ToolExecutionRecord) {
		t.Helper()
		if err := store.ModelAttempts().SaveModelAttempts(ctx, []ModelAttemptRecord{attempt}); err != nil {
			t.Fatal(err)
		}
		if err := store.ToolExecutions().SaveToolExecutions(ctx, []ToolExecutionRecord{execution}); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(
		ModelAttemptRecord{ID: "a1", RunID: "r1", ThreadID: "t1", Provider: "openai", Index: 1,
			Status: ModelAttemptFailed, StartedAt: now, FinishedAt: now},
		ToolExecutionRecord{ID: "x1", RunID: "r1", ThreadID: "t1", ToolCallID: "c1",
			ToolID: "lookup", State: ToolExecutionSucceeded, StartedAt: now, FinishedAt: now},
	)
	mustSave(
		ModelAttemptRecord{ID: "a2", RunID: "r2", ThreadID: "t2", Provider: "anthropic", Index: 1,
			Status: ModelAttemptSuccess, StartedAt: now, FinishedAt: now},
		ToolExecutionRecord{ID: "x2", RunID: "r2", ThreadID: "t2", ToolCallID: "c2",
			ToolID: "clock", State: ToolExecutionHandlerError, StartedAt: now},
	)
	for _, test := range []struct {
		name   string
		filter ModelAttemptFilter
		wantID string
	}{
		{"thread", ModelAttemptFilter{ThreadID: "t2"}, "a2"},
		{"provider", ModelAttemptFilter{Provider: "openai"}, "a1"},
		{"status", ModelAttemptFilter{Status: ModelAttemptSuccess}, "a2"},
	} {
		page, err := store.ModelAttempts().ListModelAttempts(ctx, test.filter, PageRequest{})
		if err != nil || len(page.Records) != 1 || page.Records[0].ID != test.wantID {
			t.Fatalf("%s attempt filter = %#v err=%v, want [%s]", test.name, page, err, test.wantID)
		}
	}
	for _, test := range []struct {
		name   string
		filter ToolExecutionFilter
		wantID string
	}{
		{"thread", ToolExecutionFilter{ThreadID: "t2"}, "x2"},
		{"tool", ToolExecutionFilter{ToolID: "lookup"}, "x1"},
		{"state", ToolExecutionFilter{State: ToolExecutionHandlerError}, "x2"},
	} {
		page, err := store.ToolExecutions().ListToolExecutions(ctx, test.filter, PageRequest{})
		if err != nil || len(page.Records) != 1 || page.Records[0].ID != test.wantID {
			t.Fatalf("%s tool filter = %#v err=%v, want [%s]", test.name, page, err, test.wantID)
		}
	}
}

func TestMetadataValidationEdgeCases(t *testing.T) {
	t.Parallel()

	longKey := "app." + strings.Repeat("k", maxMetadataKeyLength)
	overlong := Metadata{longKey: json.RawMessage(`1`)}
	if err := overlong.Validate(); err == nil {
		t.Fatal("overlong key accepted")
	}
	arrays := Metadata{"app.arr": json.RawMessage(`[[1,2],[3]]`)}
	if err := arrays.Validate(); err != nil {
		t.Fatalf("nested arrays rejected: %v", err)
	}
	deepArrays := Metadata{"app.deep": json.RawMessage(strings.Repeat("[", MaxMetadataDepth+2) + strings.Repeat("]", MaxMetadataDepth+2))}
	if err := deepArrays.Validate(); err == nil {
		t.Fatal("deep arrays accepted")
	}
	unicodeKey := Metadata{"app.kéy": json.RawMessage(`1`)}
	if err := unicodeKey.Validate(); err == nil {
		t.Fatal("non-ASCII key accepted")
	}
	dottedSuffix := Metadata{"app.a.b_c-d": json.RawMessage(`1`)}
	if err := dottedSuffix.Validate(); err != nil {
		t.Fatalf("dotted suffix rejected: %v", err)
	}
}

func TestObservabilityFilterAndErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	obs := store.RunEvents()
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	mustAppend := func(records []RunEventRecord) {
		t.Helper()
		if err := obs.AppendRunEvents(ctx, records); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend([]RunEventRecord{
		{ID: "e1", RunID: "r1", ThreadID: "t1", Sequence: 1, Type: RunEventStarted, Timestamp: now},
		{ID: "e2", RunID: "r2", ThreadID: "t2", Sequence: 1, Type: RunEventFailed, Timestamp: now,
			Status: RunStatusFailed, Provider: "openai", ToolID: "lookup"},
	})
	cases := []struct {
		filter RunEventFilter
		wantID string
	}{
		{RunEventFilter{ThreadID: "t2"}, "e2"},
		{RunEventFilter{Type: RunEventFailed}, "e2"},
		{RunEventFilter{Provider: "openai"}, "e2"},
		{RunEventFilter{ToolID: "lookup"}, "e2"},
		{RunEventFilter{From: now.Add(-time.Minute), To: now.Add(-1)}, ""},
	}
	for i, test := range cases {
		page, err := obs.ListRunEvents(ctx, test.filter, PageRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if test.wantID == "" {
			if len(page.Records) != 0 {
				t.Fatalf("filter %d matched %#v, want nothing", i, page.Records)
			}
			continue
		}
		if len(page.Records) != 1 || page.Records[0].ID != test.wantID {
			t.Fatalf("filter %d matched %#v, want [%s]", i, page.Records, test.wantID)
		}
	}

	// Error branches of the transactional writer surface to the caller.
	err := store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return writeObservability(ctx, repos, []RunEventRecord{{ID: "", RunID: "", Sequence: 0}}, nil, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "append run events") {
		t.Fatalf("event write error = %v", err)
	}
	err = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return writeObservability(ctx, repos, nil, []ModelAttemptRecord{{ID: "bad"}}, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "save model attempts") {
		t.Fatalf("attempt write error = %v", err)
	}
	err = store.Transaction(ctx, func(ctx context.Context, repos Repositories) error {
		return writeObservability(ctx, repos, nil, nil, []ToolExecutionRecord{{ID: "bad"}})
	})
	if err == nil || !strings.Contains(err.Error(), "save tool executions") {
		t.Fatalf("tool write error = %v", err)
	}

	// Nil-receiver helpers are no-ops.
	var nilJournal *runJournal
	nilJournal.markPersisted(1, 2, 3)
	nilEvents, nilAttempts, nilTools := nilJournal.snapshot()
	if nilEvents != nil || nilAttempts != nil || nilTools != nil {
		t.Fatal("nil journal snapshot returned records")
	}
}

func TestLinkProducedMessagesWithoutWinner(t *testing.T) {
	t.Parallel()
	journal := newRunJournal(NewFixedClock(time.Unix(0, 0)), NewMemoryStore(), "r", "", ObservabilityScope{}, nil)
	journal.completeModelAttempt(ModelAttempt{Provider: "p", Status: ModelAttemptFailed})
	journal.linkProducedMessages([]string{"m-1"})
	events, attempts, _ := journal.snapshot()
	if len(events) != 0 && len(attempts) != 1 {
		t.Fatalf("unexpected snapshot shape")
	}
	if attempts[0].ProducedMessageIDs != nil {
		t.Fatalf("failed attempt claimed produced messages: %v", attempts[0].ProducedMessageIDs)
	}
}

func TestClassifyAndJournalEdgeBranches(t *testing.T) {
	t.Parallel()

	toolErr := &ToolExecutionError{ToolID: "lookup", State: ToolExecutionPanicked, Err: errors.New("boom")}
	kind, message := classifyRunError(toolErr)
	if kind != string(ToolExecutionPanicked) || message != "redacted" {
		t.Fatalf("tool error classification = %q/%q", kind, message)
	}

	var nilJournal *runJournal
	nilJournal.OnRunEvent(RunEvent{Sequence: 1, Type: RunEventStarted})
	nilJournal.linkProducedMessages([]string{"m"})

	// A payload-bearing event keeps its payload through capture.
	journal := newRunJournal(NewFixedClock(time.Unix(0, 0)), NewMemoryStore(), "r", "", ObservabilityScope{}, nil)
	journal.OnRunEvent(RunEvent{Sequence: 5, Type: RunEventStepAttemptFinished, Attempt: 2,
		Delay: time.Second, Timestamp: time.Unix(0, 0), Error: context.DeadlineExceeded})
	events, _, _ := journal.snapshot()
	if len(events) != 1 || events[0].ErrorKind != "deadline_exceeded" {
		t.Fatalf("captured event = %#v", events)
	}
	if string(events[0].Payload) != `{"attempt":2,"delay_ns":1000000000}` {
		t.Fatalf("payload = %s", events[0].Payload)
	}
}

func TestValidatorRejectionOrder(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	zeroIndex := ModelAttemptRecord{ID: "a", RunID: "r", Index: 0, Status: ModelAttemptSuccess,
		StartedAt: now, FinishedAt: now}
	if validateModelAttemptRecord(zeroIndex) == nil {
		t.Fatal("zero index accepted")
	}
	badStatus := ModelAttemptRecord{ID: "a", RunID: "r", Index: 1, Status: "weird",
		StartedAt: now, FinishedAt: now}
	if validateModelAttemptRecord(badStatus) == nil {
		t.Fatal("invalid status accepted")
	}
	missingTime := ModelAttemptRecord{ID: "a", RunID: "r", Index: 1, Status: ModelAttemptSuccess,
		StartedAt: now}
	if validateModelAttemptRecord(missingTime) == nil {
		t.Fatal("missing finish time accepted")
	}
	badState := ToolExecutionRecord{ID: "t", RunID: "r", ToolCallID: "c", ToolID: "tool",
		State: "exploded", StartedAt: now}
	if validateToolExecutionRecord(badState) == nil {
		t.Fatal("invalid state accepted")
	}
	missingTimestamp := RunEventRecord{ID: "e", RunID: "r", Sequence: 1, Type: RunEventStarted}
	if validateRunEventRecord(missingTimestamp) == nil {
		t.Fatal("missing timestamp accepted")
	}

	// A namespace with invalid characters is rejected distinctly from a
	// missing namespace.
	if err := (Metadata{"a p.x": json.RawMessage(`1`)}).Validate(); err == nil {
		t.Fatal("namespace with spaces accepted")
	}
}

func TestEncodingHelperFallbacks(t *testing.T) {
	t.Parallel()

	if got := obsMetadataJSON(Metadata{"app.x": json.RawMessage(`{`)}); got != nil {
		t.Fatalf("corrupt metadata encode = %v, want nil", got)
	}
	values := obsStringArray([]string{"a", "b"})
	if values != `["a","b"]` {
		t.Fatalf("string array = %v", values)
	}

	toolNoID := ToolExecutionRecord{ID: "t", RunID: "r", ToolCallID: "c", State: ToolExecutionSucceeded, StartedAt: time.Now()}
	if validateToolExecutionRecord(toolNoID) == nil {
		t.Fatal("missing tool ID accepted")
	}

	journal := newRunJournal(NewFixedClock(time.Unix(0, 0)), NewMemoryStore(), "r", "", ObservabilityScope{}, nil)
	journal.OnRunEvent(RunEvent{Sequence: 6, Type: RunEventRouteSelected, Timestamp: time.Unix(0, 0)})
	events, _, _ := journal.snapshot()
	if len(events) != 1 || events[0].Payload != nil {
		t.Fatalf("empty route payload = %#v", events[0].Payload)
	}

	if got := jsonDepth([]byte(`not json`)); got != 1 {
		t.Fatalf("invalid JSON depth = %d, want scalar fallback of 1", got)
	}
}

func TestSQLiteAppendRunEventsChunksLargeBatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := NewSQLiteStore(t.TempDir() + "/chunks.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	const total = 1200 // exceeds the 500-row insert chunk size
	now := time.Now().UTC()
	records := make([]RunEventRecord, 0, total)
	for i := 0; i < total; i++ {
		records = append(records, RunEventRecord{ID: fmt.Sprintf("e-%d", i), RunID: "run-chunk",
			ThreadID: "thread-chunk", Sequence: int64(i + 1), Type: RunEventStarted,
			Timestamp: now.Add(time.Duration(i) * time.Millisecond)})
	}
	if err := store.RunEvents().AppendRunEvents(ctx, records); err != nil {
		t.Fatal(err)
	}
	page, err := store.RunEvents().ListRunEvents(ctx, RunEventFilter{ThreadID: "thread-chunk"}, PageRequest{Limit: total})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != total {
		t.Fatalf("chunked append stored %d of %d events", len(page.Records), total)
	}
	if page.Records[0].Sequence != 1 || page.Records[total-1].Sequence != total {
		t.Fatalf("chunked append lost ordering: first=%d last=%d", page.Records[0].Sequence, page.Records[total-1].Sequence)
	}
}
