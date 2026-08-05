package lebro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// ToolDefinition describes a callable capability for a model. Schemas are raw
// JSON so the core package does not depend on a particular JSON Schema library.
type ToolDefinition struct {
	ID           ToolID
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// Tool is implemented by application capabilities. Register tools with a
// ToolRegistry to enforce their schema boundary during execution.
type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, json.RawMessage) (json.RawMessage, error)
}

// ToolExecutionRequest is one schema-checked invocation. Metadata is copied
// into the handler context and can be read with ToolMetadataFromContext.
type ToolExecutionRequest struct {
	Arguments json.RawMessage
	Metadata  map[string]string
}

// ToolExecutionState identifies the distinct outcome of a tool invocation.
type ToolExecutionState string

const (
	ToolExecutionSucceeded     ToolExecutionState = "succeeded"
	ToolExecutionInvalidInput  ToolExecutionState = "invalid_input"
	ToolExecutionInvalidOutput ToolExecutionState = "invalid_output"
	ToolExecutionHandlerError  ToolExecutionState = "handler_error"
	ToolExecutionPanicked      ToolExecutionState = "panicked"
	ToolExecutionCancelled     ToolExecutionState = "cancelled"
	ToolExecutionNotFound      ToolExecutionState = "not_found"
)

// ToolExecutionResult is the normalized result of a tool invocation. Output is
// populated only when State is ToolExecutionSucceeded. Err is a
// *ToolExecutionError for every non-success state.
type ToolExecutionResult struct {
	ToolID ToolID
	State  ToolExecutionState
	Output json.RawMessage
	Err    error
}

// ToolExecutionError associates a failure with its tool and result state.
type ToolExecutionError struct {
	ToolID ToolID
	State  ToolExecutionState
	Err    error
}

func (e *ToolExecutionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro: tool %q execution %s", e.ToolID, e.State)
	}
	return fmt.Sprintf("lebro: tool %q execution %s: %v", e.ToolID, e.State, e.Err)
}

// Unwrap exposes the validation, cancellation, panic, or handler error.
func (e *ToolExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ToolPanicError records a recovered handler panic without allowing the panic
// value's formatting methods to trigger another panic.
type ToolPanicError struct {
	Value any
}

func (e *ToolPanicError) Error() string {
	if e == nil {
		return ""
	}
	if message, ok := e.Value.(string); ok {
		return "lebro: tool handler panicked: " + message
	}
	return fmt.Sprintf("lebro: tool handler panicked with %T", e.Value)
}

// ErrToolNotFound is wrapped when an unregistered tool ID is invoked.
var ErrToolNotFound = errors.New("lebro: tool not found")

// RegisteredTool is an immutable, schema-backed tool resolved from a registry.
// Its Execute method is the safe invocation boundary.
type RegisteredTool struct {
	definition ToolDefinition
	handler    Tool
	validator  *ToolSchemaValidator
}

// Definition returns a caller-owned copy of the registered definition.
func (t *RegisteredTool) Definition() ToolDefinition {
	if t == nil {
		return ToolDefinition{}
	}
	return cloneToolDefinition(t.definition)
}

// Execute validates arguments, invokes the handler with request metadata, and
// validates the returned value. Handler panics and errors are normalized.
func (t *RegisteredTool) Execute(ctx context.Context, request ToolExecutionRequest) ToolExecutionResult {
	if t == nil {
		return failedToolExecution("", ToolExecutionNotFound, ErrToolNotFound)
	}

	id := t.definition.ID
	if ctx == nil {
		return failedToolExecution(id, ToolExecutionHandlerError, errors.New("lebro: tool context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return failedToolExecution(id, ToolExecutionCancelled, err)
	}
	if err := t.validator.ValidateInput(request.Arguments); err != nil {
		return failedToolExecution(id, ToolExecutionInvalidInput, err)
	}

	handlerCtx := context.WithValue(ctx, toolMetadataContextKey{}, cloneMetadata(request.Metadata))
	output, handlerErr, panicValue, panicked := invokeToolHandler(handlerCtx, t.handler, cloneRawMessage(request.Arguments))
	if panicked {
		return failedToolExecution(id, ToolExecutionPanicked, &ToolPanicError{Value: panicValue})
	}
	if err := ctx.Err(); err != nil {
		return failedToolExecution(id, ToolExecutionCancelled, err)
	}
	if handlerErr != nil {
		if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
			return failedToolExecution(id, ToolExecutionCancelled, handlerErr)
		}
		return failedToolExecution(id, ToolExecutionHandlerError, handlerErr)
	}
	if err := t.validator.ValidateOutput(output); err != nil {
		return failedToolExecution(id, ToolExecutionInvalidOutput, err)
	}

	return ToolExecutionResult{
		ToolID: id,
		State:  ToolExecutionSucceeded,
		Output: cloneRawMessage(output),
	}
}

// ToolRegistry compiles tool schemas once and resolves immutable safe execution
// boundaries by stable ID. Registration and resolution are concurrency-safe.
type ToolRegistry struct {
	compiler   SchemaCompiler
	compilerMu sync.Mutex
	mu         sync.RWMutex
	tools      map[ToolID]*RegisteredTool
}

// NewToolRegistry creates an empty registry using compiler for both schema
// boundaries of every registered tool.
func NewToolRegistry(compiler SchemaCompiler) (*ToolRegistry, error) {
	if compiler == nil {
		return nil, errors.New("lebro: schema compiler is required")
	}
	return &ToolRegistry{compiler: compiler, tools: make(map[ToolID]*RegisteredTool)}, nil
}

// Register validates and compiles a tool definition before publishing it.
// Existing stable IDs cannot be replaced implicitly.
func (r *ToolRegistry) Register(tool Tool) error {
	if r == nil {
		return errors.New("lebro: tool registry is nil")
	}
	if tool == nil || isNilTool(tool) {
		return errors.New("lebro: tool is required")
	}

	definition := cloneToolDefinition(tool.Definition())
	if definition.ID == "" {
		return errors.New("lebro: tool ID is required")
	}
	if strings.TrimSpace(string(definition.ID)) != string(definition.ID) {
		return fmt.Errorf("lebro: tool ID %q must not have surrounding whitespace", definition.ID)
	}

	validator, err := r.compileToolSchemas(definition)
	if err != nil {
		return fmt.Errorf("lebro: register tool %q: %w", definition.ID, err)
	}
	registered := &RegisteredTool{definition: definition, handler: tool, validator: validator}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[definition.ID]; exists {
		return fmt.Errorf("lebro: tool ID %q is already registered", definition.ID)
	}
	r.tools[definition.ID] = registered
	return nil
}

// Resolve returns the immutable safe execution boundary registered for id.
func (r *ToolRegistry) Resolve(id ToolID) (*RegisteredTool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[id]
	return tool, ok
}

// Definitions returns caller-owned definitions ordered by stable ID.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	definitions := make([]ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, cloneToolDefinition(tool.definition))
	}
	r.mu.RUnlock()
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].ID < definitions[j].ID })
	return definitions
}

// Execute resolves id and invokes its safe schema boundary.
func (r *ToolRegistry) Execute(ctx context.Context, id ToolID, request ToolExecutionRequest) ToolExecutionResult {
	tool, ok := r.Resolve(id)
	if !ok {
		return failedToolExecution(id, ToolExecutionNotFound, ErrToolNotFound)
	}
	return tool.Execute(ctx, request)
}

func (r *ToolRegistry) compileToolSchemas(definition ToolDefinition) (*ToolSchemaValidator, error) {
	r.compilerMu.Lock()
	defer r.compilerMu.Unlock()
	return NewToolSchemaValidator(r.compiler, definition)
}

type toolMetadataContextKey struct{}

// ToolMetadataFromContext returns a caller-owned copy of request-scoped tool
// metadata. It returns nil outside a registered tool invocation.
func ToolMetadataFromContext(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	metadata, _ := ctx.Value(toolMetadataContextKey{}).(map[string]string)
	return cloneMetadata(metadata)
}

func invokeToolHandler(ctx context.Context, handler Tool, input json.RawMessage) (output json.RawMessage, err error, panicValue any, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			output = nil
			err = nil
			panicValue = recovered
			panicked = true
		}
	}()
	output, err = handler.Execute(ctx, input)
	return output, err, nil, false
}

func failedToolExecution(id ToolID, state ToolExecutionState, err error) ToolExecutionResult {
	return ToolExecutionResult{
		ToolID: id,
		State:  state,
		Err:    &ToolExecutionError{ToolID: id, State: state, Err: err},
	}
}

func cloneToolDefinition(definition ToolDefinition) ToolDefinition {
	definition.InputSchema = cloneRawMessage(definition.InputSchema)
	definition.OutputSchema = cloneRawMessage(definition.OutputSchema)
	return definition
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func isNilTool(tool Tool) bool {
	value := reflect.ValueOf(tool)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
