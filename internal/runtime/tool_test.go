package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type toolFunc struct {
	definition ToolDefinition
	execute    func(context.Context, json.RawMessage) (json.RawMessage, error)
}

type pointerTool struct{}

func (*pointerTool) Definition() ToolDefinition { return ToolDefinition{ID: "pointer"} }

func (*pointerTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

type pointerSchemaCompiler struct{}

func (*pointerSchemaCompiler) Compile(json.RawMessage) (CompiledSchema, error) {
	return stubCompiledSchema{}, nil
}

func (t toolFunc) Definition() ToolDefinition { return t.definition }

func (t toolFunc) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return t.execute(ctx, input)
}

type validatingSchema struct {
	validate func(json.RawMessage) *ValidationError
}

func (s validatingSchema) Validate(value json.RawMessage) *ValidationError {
	return s.validate(value)
}

func TestToolRegistryExecutesSchemaBackedTool(t *testing.T) {
	t.Parallel()

	inputSchema := json.RawMessage(`{"title":"input"}`)
	outputSchema := json.RawMessage(`{"title":"output"}`)
	compiler := stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		want := `{"ok":true}`
		if string(schema) == string(inputSchema) {
			want = `{"name":"Ada"}`
		}
		return validatingSchema{validate: func(value json.RawMessage) *ValidationError {
			if string(value) == want {
				return nil
			}
			return &ValidationError{Issues: []ValidationIssue{{Path: "/", Keyword: "const", Message: "unexpected value"}}}
		}}, nil
	}}
	registry, err := NewToolRegistry(compiler)
	if err != nil {
		t.Fatal(err)
	}

	var receivedInput string
	var receivedMetadata map[string]string
	output := json.RawMessage(`{"ok":true}`)
	definition := ToolDefinition{ID: "greet", Description: "Greets a person", InputSchema: inputSchema, OutputSchema: outputSchema}
	tool := toolFunc{definition: definition, execute: func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		receivedInput = string(input)
		input[0] = '['
		receivedMetadata = ToolMetadataFromContext(ctx)
		receivedMetadata["request"] = "changed"
		return output, nil
	}}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}

	definition.InputSchema[0] = '['
	definition.OutputSchema[0] = '['
	arguments := json.RawMessage(`{"name":"Ada"}`)
	metadata := map[string]string{"request": "req-1"}
	result := registry.Execute(context.Background(), "greet", ToolExecutionRequest{Arguments: arguments, Metadata: metadata})
	if result.State != ToolExecutionSucceeded || result.ToolID != "greet" || result.Err != nil || string(result.Output) != `{"ok":true}` {
		t.Fatalf("result = %#v", result)
	}
	if receivedInput != `{"name":"Ada"}` || string(arguments) != `{"name":"Ada"}` {
		t.Fatalf("received input = %q, original = %s", receivedInput, arguments)
	}
	if receivedMetadata["request"] != "changed" || metadata["request"] != "req-1" {
		t.Fatalf("received metadata = %#v, original = %#v", receivedMetadata, metadata)
	}
	output[0] = '['
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("result output changed to %s", result.Output)
	}

	registered, ok := registry.Resolve("greet")
	if !ok {
		t.Fatal("registered tool was not resolved")
	}
	resolvedDefinition := registered.Definition()
	if resolvedDefinition.ID != "greet" || string(resolvedDefinition.InputSchema) != `{"title":"input"}` {
		t.Fatalf("resolved definition = %#v", resolvedDefinition)
	}
	resolvedDefinition.InputSchema[0] = '['
	if got := registered.Definition(); string(got.InputSchema) != `{"title":"input"}` {
		t.Fatalf("registered definition mutated: %#v", got)
	}
}

func TestToolRegistryRejectsInvalidInputWithoutCallingHandler(t *testing.T) {
	t.Parallel()

	registry := registryForToolTest(t, validatingSchema{validate: func(json.RawMessage) *ValidationError {
		return &ValidationError{Issues: []ValidationIssue{{Message: "invalid arguments"}}}
	}}, nil)
	called := false
	registerToolForTest(t, registry, ToolDefinition{ID: "invalid", InputSchema: json.RawMessage(`{}`)}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		called = true
		return nil, nil
	})

	result := registry.Execute(context.Background(), "invalid", ToolExecutionRequest{Arguments: json.RawMessage(`null`)})
	assertToolState(t, result, ToolExecutionInvalidInput)
	if called {
		t.Fatal("handler received invalid arguments")
	}
	var validationErr *ValidationError
	if !errors.As(result.Err, &validationErr) || validationErr.Target != ValidationTargetToolInput {
		t.Fatalf("error = %#v", result.Err)
	}
}

func TestToolRegistryNormalizesInvalidOutputAndHandlerFailures(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("business rule failed")
	tests := []struct {
		name       string
		execute    func(context.Context, json.RawMessage) (json.RawMessage, error)
		wantState  ToolExecutionState
		wantError  error
		wantPanic  bool
		panicValue any
	}{
		{name: "invalid output", execute: func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`false`), nil }, wantState: ToolExecutionInvalidOutput},
		{name: "handler error", execute: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, handlerErr }, wantState: ToolExecutionHandlerError, wantError: handlerErr},
		{name: "canceled error", execute: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, context.Canceled }, wantState: ToolExecutionCancelled, wantError: context.Canceled},
		{name: "deadline error", execute: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, context.DeadlineExceeded }, wantState: ToolExecutionCancelled, wantError: context.DeadlineExceeded},
		{name: "string panic", execute: func(context.Context, json.RawMessage) (json.RawMessage, error) { panic("boom") }, wantState: ToolExecutionPanicked, wantPanic: true, panicValue: "boom"},
		{name: "typed panic", execute: func(context.Context, json.RawMessage) (json.RawMessage, error) { panic(42) }, wantState: ToolExecutionPanicked, wantPanic: true, panicValue: 42},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputValidation := validatingSchema{validate: func(value json.RawMessage) *ValidationError {
				if string(value) == `true` {
					return nil
				}
				return &ValidationError{Issues: []ValidationIssue{{Message: "invalid result"}}}
			}}
			registry := registryForToolTest(t, outputValidation, nil)
			registerToolForTest(t, registry, ToolDefinition{ID: "run", OutputSchema: json.RawMessage(`{}`)}, test.execute)

			result := registry.Execute(context.Background(), "run", ToolExecutionRequest{})
			assertToolState(t, result, test.wantState)
			if test.wantError != nil && !errors.Is(result.Err, test.wantError) {
				t.Fatalf("error = %v, want %v", result.Err, test.wantError)
			}
			if test.wantState == ToolExecutionInvalidOutput {
				var validationErr *ValidationError
				if !errors.As(result.Err, &validationErr) || validationErr.Target != ValidationTargetToolOutput {
					t.Fatalf("error = %#v", result.Err)
				}
			}
			if test.wantPanic {
				var panicErr *ToolPanicError
				if !errors.As(result.Err, &panicErr) || !reflect.DeepEqual(panicErr.Value, test.panicValue) {
					t.Fatalf("panic error = %#v", panicErr)
				}
				if !strings.Contains(panicErr.Error(), "panicked") {
					t.Fatalf("panic error message = %q", panicErr.Error())
				}
			}
		})
	}
}

func TestToolRegistryHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	registry := registryForToolTest(t, nil, nil)
	called := false
	registerToolForTest(t, registry, ToolDefinition{ID: "cancel"}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		called = true
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := registry.Execute(ctx, "cancel", ToolExecutionRequest{})
	assertToolState(t, result, ToolExecutionCancelled)
	if called || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("called = %v, error = %v", called, result.Err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	registry = registryForToolTest(t, nil, nil)
	registerToolForTest(t, registry, ToolDefinition{ID: "during"}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		cancel()
		return json.RawMessage(`null`), nil
	})
	result = registry.Execute(ctx, "during", ToolExecutionRequest{})
	assertToolState(t, result, ToolExecutionCancelled)

	ctx, cancel = context.WithCancel(context.Background())
	registry = registryForToolTest(t, validatingSchema{validate: func(json.RawMessage) *ValidationError {
		cancel()
		return nil
	}}, nil)
	called = false
	registerToolForTest(t, registry, ToolDefinition{ID: "input-validation", InputSchema: json.RawMessage(`{}`)}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		called = true
		return nil, nil
	})
	result = registry.Execute(ctx, "input-validation", ToolExecutionRequest{})
	assertToolState(t, result, ToolExecutionCancelled)
	if called {
		t.Fatal("handler called after cancellation during input validation")
	}

	ctx, cancel = context.WithCancel(context.Background())
	registry = registryForToolTest(t, validatingSchema{validate: func(json.RawMessage) *ValidationError {
		cancel()
		return nil
	}}, nil)
	registerToolForTest(t, registry, ToolDefinition{ID: "output-validation", OutputSchema: json.RawMessage(`{}`)}, echoToolHandler)
	result = registry.Execute(ctx, "output-validation", ToolExecutionRequest{})
	assertToolState(t, result, ToolExecutionCancelled)

	ctx, cancel = context.WithCancel(context.Background())
	registry = registryForToolTest(t, nil, nil)
	registerToolForTest(t, registry, ToolDefinition{ID: "panic-cancel"}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		cancel()
		panic("boom")
	})
	result = registry.Execute(ctx, "panic-cancel", ToolExecutionRequest{})
	assertToolState(t, result, ToolExecutionCancelled)
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("panic during cancellation error = %v, want context.Canceled", result.Err)
	}
}

func TestToolRegistryRegistrationAndLookup(t *testing.T) {
	t.Parallel()

	if _, err := NewToolRegistry(nil); err == nil {
		t.Fatal("nil compiler accepted")
	}
	var typedNilCompiler *pointerSchemaCompiler
	if _, err := NewToolRegistry(typedNilCompiler); err == nil {
		t.Fatal("typed nil compiler accepted")
	}
	registry := registryForToolTest(t, nil, nil)
	registerToolForTest(t, registry, ToolDefinition{ID: "z"}, echoToolHandler)
	registerToolForTest(t, registry, ToolDefinition{ID: "a"}, echoToolHandler)

	definitions := registry.Definitions()
	if got := []ToolID{definitions[0].ID, definitions[1].ID}; !reflect.DeepEqual(got, []ToolID{"a", "z"}) {
		t.Fatalf("definition IDs = %v", got)
	}
	if _, ok := registry.Resolve("missing"); ok {
		t.Fatal("missing tool resolved")
	}
	result := registry.Execute(context.Background(), "missing", ToolExecutionRequest{})
	assertToolState(t, result, ToolExecutionNotFound)
	if !errors.Is(result.Err, ErrToolNotFound) {
		t.Fatalf("error = %v", result.Err)
	}

	if err := registry.Register(toolFunc{definition: ToolDefinition{ID: "a"}, execute: echoToolHandler}); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	if err := registry.Register(toolFunc{execute: echoToolHandler}); err == nil {
		t.Fatal("empty ID accepted")
	}
	if err := registry.Register(toolFunc{definition: ToolDefinition{ID: " padded "}, execute: echoToolHandler}); err == nil {
		t.Fatal("padded ID accepted")
	}
	if err := registry.Register(nil); err == nil {
		t.Fatal("nil tool accepted")
	}
	var typedNil *pointerTool
	if err := registry.Register(typedNil); err == nil {
		t.Fatal("typed nil tool accepted")
	}

	badRegistry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return nil, errors.New("cannot compile")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := badRegistry.Register(toolFunc{definition: ToolDefinition{ID: "bad", InputSchema: json.RawMessage(`{}`)}, execute: echoToolHandler}); err == nil || !strings.Contains(err.Error(), `register tool "bad"`) {
		t.Fatalf("compile error = %v", err)
	}

	goodRegistry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return nil, errors.New("cannot compile")
	}})
	if err != nil {
		t.Fatal(err)
	}
	goodRegistry.tools["dup"] = &RegisteredTool{definition: ToolDefinition{ID: "dup"}}
	dupErr := goodRegistry.Register(toolFunc{definition: ToolDefinition{ID: "dup", InputSchema: json.RawMessage(`{}`)}, execute: echoToolHandler})
	if dupErr == nil {
		t.Fatal("duplicate ID accepted")
	}
	if !strings.Contains(dupErr.Error(), `is already registered`) {
		t.Fatalf("duplicate registration error = %v, want already registered", dupErr)
	}
	if strings.Contains(dupErr.Error(), `register tool "dup"`) {
		t.Fatalf("duplicate registration ran schema compile: %v", dupErr)
	}
}

func TestToolRegistrySupportsConcurrentRegistration(t *testing.T) {
	t.Parallel()

	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		for range 100 {
			runtime.Gosched()
		}
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}

	const toolCount = 16
	errorsByTool := make(chan error, toolCount)
	var registrations sync.WaitGroup
	for index := range toolCount {
		registrations.Add(1)
		go func() {
			defer registrations.Done()
			errorsByTool <- registry.Register(toolFunc{
				definition: ToolDefinition{ID: ToolID(string(rune('a' + index))), InputSchema: json.RawMessage(`{}`)},
				execute:    echoToolHandler,
			})
		}()
	}
	registrations.Wait()
	close(errorsByTool)
	for err := range errorsByTool {
		if err != nil {
			t.Fatal(err)
		}
	}
	if definitions := registry.Definitions(); len(definitions) != toolCount {
		t.Fatalf("registered %d tools, want %d", len(definitions), toolCount)
	}
}

func TestToolRegistryKeepsLookupsAvailableDuringCompilation(t *testing.T) {
	t.Parallel()

	compileStarted := make(chan struct{})
	finishCompile := make(chan struct{})
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		close(compileStarted)
		<-finishCompile
		return stubCompiledSchema{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	registry.tools["existing"] = &RegisteredTool{definition: ToolDefinition{ID: "existing"}}

	registered := make(chan error, 1)
	go func() {
		registered <- registry.Register(toolFunc{
			definition: ToolDefinition{ID: "slow", InputSchema: json.RawMessage(`{}`)},
			execute:    echoToolHandler,
		})
	}()
	<-compileStarted

	resolved := make(chan bool, 1)
	go func() {
		_, ok := registry.Resolve("existing")
		resolved <- ok
	}()
	select {
	case ok := <-resolved:
		if !ok {
			t.Fatal("existing tool was not resolved")
		}
	case <-time.After(time.Second):
		t.Fatal("lookup blocked during schema compilation")
	}

	close(finishCompile)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
}

func TestNilToolRegistryAndRegisteredTool(t *testing.T) {
	t.Parallel()

	var registry *ToolRegistry
	if err := registry.Register(toolFunc{}); err == nil {
		t.Fatal("nil registry accepted registration")
	}
	if definitions := registry.Definitions(); definitions != nil {
		t.Fatalf("definitions = %#v", definitions)
	}
	if tool, ok := registry.Resolve("anything"); ok || tool != nil {
		t.Fatalf("resolved = %#v, %v", tool, ok)
	}
	assertToolState(t, registry.Execute(context.Background(), "anything", ToolExecutionRequest{}), ToolExecutionNotFound)

	var registered *RegisteredTool
	if definition := registered.Definition(); definition.ID != "" {
		t.Fatalf("definition = %#v", definition)
	}
	assertToolState(t, registered.Execute(context.Background(), ToolExecutionRequest{}), ToolExecutionNotFound)

	registry = registryForToolTest(t, nil, nil)
	registerToolForTest(t, registry, ToolDefinition{ID: "nil-context"}, echoToolHandler)
	assertToolState(t, registry.Execute(nilContextForTest(), "nil-context", ToolExecutionRequest{}), ToolExecutionHandlerError)
}

func TestToolExecutionErrorsAndMetadata(t *testing.T) {
	t.Parallel()

	plain := &ToolExecutionError{ToolID: "plain", State: ToolExecutionHandlerError}
	if got := plain.Error(); got != `lebro: tool "plain" execution handler_error` {
		t.Fatalf("Error() = %q", got)
	}
	if plain.Unwrap() != nil {
		t.Fatalf("Unwrap() = %v", plain.Unwrap())
	}
	wrapped := &ToolExecutionError{ToolID: "wrapped", State: ToolExecutionHandlerError, Err: errors.New("failed")}
	if got := wrapped.Error(); !strings.Contains(got, "failed") {
		t.Fatalf("wrapped Error() = %q", got)
	}
	var nilExecutionErr *ToolExecutionError
	if nilExecutionErr.Error() != "" || nilExecutionErr.Unwrap() != nil {
		t.Fatal("nil ToolExecutionError is not safe")
	}
	var nilPanicErr *ToolPanicError
	if nilPanicErr.Error() != "" {
		t.Fatalf("nil ToolPanicError = %q", nilPanicErr.Error())
	}
	if got := (&ToolPanicError{Value: errors.New("secret")}).Error(); !strings.Contains(got, "*errors.errorString") {
		t.Fatalf("typed panic message = %q", got)
	}
	if metadata := ToolMetadataFromContext(context.Background()); metadata != nil {
		t.Fatalf("background metadata = %#v", metadata)
	}
	if metadata := ToolMetadataFromContext(nilContextForTest()); metadata != nil {
		t.Fatalf("nil context metadata = %#v", metadata)
	}
}

func registryForToolTest(t *testing.T, compiled CompiledSchema, compileErr error) *ToolRegistry {
	t.Helper()
	registry, err := NewToolRegistry(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return compiled, compileErr
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func registerToolForTest(t *testing.T, registry *ToolRegistry, definition ToolDefinition, execute func(context.Context, json.RawMessage) (json.RawMessage, error)) {
	t.Helper()
	if err := registry.Register(toolFunc{definition: definition, execute: execute}); err != nil {
		t.Fatal(err)
	}
}

func echoToolHandler(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return input, nil
}

func assertToolState(t *testing.T, result ToolExecutionResult, want ToolExecutionState) {
	t.Helper()
	if result.State != want || result.Err == nil || result.Output != nil {
		t.Fatalf("result = %#v, want state %q with error and no output", result, want)
	}
	var executionErr *ToolExecutionError
	if !errors.As(result.Err, &executionErr) || executionErr.State != want || executionErr.ToolID != result.ToolID {
		t.Fatalf("execution error = %#v", result.Err)
	}
}

func nilContextForTest() context.Context {
	return nil
}
