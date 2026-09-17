package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// RequestResolver resolves one caller's currently granted MCP capabilities.
// It is called for every inbound HTTP request, including tools/list and
// tools/call, so revoked grants cannot be reused from an earlier list result.
// Authentication and tenant policy remain application responsibilities.
type RequestResolver func(*http.Request) (RequestExposure, error)

// RequestExposure is the complete allow-list for one inbound HTTP request.
// Each adapter is independent from an in-memory lebro runtime object, allowing
// applications to dispatch to persisted published definitions.
type RequestExposure struct {
	Tools     []ToolAdapter
	Agents    []AgentAdapter
	Workflows []WorkflowAdapter
}

// ToolAdapter describes and executes one persisted or remote tool. Execute
// receives the request-derived context and must return a normalized result.
type ToolAdapter struct {
	Definition lebro.ToolDefinition
	Execute    func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult
}

// AgentAdapter describes and executes one persisted agent.
type AgentAdapter struct {
	Definition lebro.WorkflowDefinition
	Run        func(context.Context, lebro.RunInput) (lebro.RunResult, error)
}

// WorkflowAdapter describes and executes one persisted workflow. ValidateInput
// is required when InputSchema is set and enforces the persisted definition's
// input boundary before Run.
type WorkflowAdapter struct {
	Definition    lebro.WorkflowDefinition
	InputSchema   json.RawMessage
	ValidateInput func(json.RawMessage) error
	Run           func(context.Context, lebro.WorkflowRunInput) (lebro.WorkflowRunResult, error)
}

type scopedServerContextKey struct{}

type toolValidators struct {
	input  lebro.CompiledSchema
	output lebro.CompiledSchema
}

func (s *Server) requestScopedHTTPHandler(opts *mcpsdk.StreamableHTTPOptions) http.Handler {
	if s.config.RequestResolver == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "MCP request resolver is required", http.StatusInternalServerError)
		})
	}
	if opts == nil {
		opts = &mcpsdk.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true}
	} else {
		copied := *opts
		opts = &copied
	}
	// A scoped server is rebuilt per request. Stateful MCP sessions would bind
	// later requests to a server created for a previous principal, so this path
	// is always stateless.
	opts.Stateless = true
	opts.PropagateRequestCancellation = true
	transport := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		server, _ := r.Context().Value(scopedServerContextKey{}).(*mcpsdk.Server)
		return server
	}, opts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exposure, err := s.config.RequestResolver(r)
		if err != nil {
			slog.Error("lebro/mcp: resolve request-scoped capabilities", "error", err)
			writeResolutionError(w, err)
			return
		}
		server, err := s.serverForExposure(exposure)
		if err != nil {
			slog.Error("lebro/mcp: build request-scoped server", "error", err)
			http.Error(w, "MCP capability resolution failed", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), scopedServerContextKey{}, server)
		transport.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeResolutionError(w http.ResponseWriter, err error) {
	var resolutionErr *RequestResolutionError
	if errors.As(err, &resolutionErr) {
		status := resolutionErr.StatusCode
		if status < http.StatusBadRequest || status > 599 {
			status = http.StatusInternalServerError
		}
		message := resolutionErr.Message
		if message == "" {
			message = "MCP capability resolution failed"
		}
		http.Error(w, message, status)
		return
	}
	http.Error(w, "MCP capability resolution failed", http.StatusInternalServerError)
}

// RequestResolutionError returns a stable public failure from RequestResolver.
// Do not put sensitive authentication or tenant details in Message.
type RequestResolutionError struct {
	StatusCode int
	Message    string
}

func (e *RequestResolutionError) Error() string { return e.Message }

func (s *Server) serverForExposure(exposure RequestExposure) (*mcpsdk.Server, error) {
	config := s.config
	config.RequestResolver = nil
	server := NewServer(config)
	server.validators = s.validators
	for _, adapter := range exposure.Tools {
		if err := server.ExposeToolAdapter(adapter); err != nil {
			return nil, err
		}
	}
	for _, adapter := range exposure.Agents {
		if err := server.ExposeAgentAdapter(adapter); err != nil {
			return nil, err
		}
	}
	for _, adapter := range exposure.Workflows {
		if err := server.ExposeWorkflowAdapter(adapter); err != nil {
			return nil, err
		}
	}
	return server.mcpServer, nil
}

// ExposeToolAdapter registers a context-aware tool execution adapter. Use it
// when the application stores the capability separately from a ToolRegistry.
func (s *Server) ExposeToolAdapter(adapter ToolAdapter) error {
	if s.config.RequestResolver != nil {
		return errors.New("lebro/mcp: expose adapters through RequestResolver when request-scoped exposure is configured")
	}
	if adapter.Execute == nil {
		return errors.New("lebro/mcp: tool adapter Execute is required")
	}
	validators, err := s.toolAdapterValidators(adapter.Definition)
	if err != nil {
		return err
	}
	return s.exposeTool(adapter.Definition, adapter.Execute, validators)
}

func (s *Server) toolAdapterValidators(def lebro.ToolDefinition) (*toolValidators, error) {
	inputSchema, err := normalizeInputSchema(def.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("lebro/mcp: tool %q: %w", def.ID, err)
	}
	var outputSchema json.RawMessage
	if len(def.OutputSchema) > 0 {
		outputSchema, err = normalizeOutputSchema(def.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("lebro/mcp: tool %q: %w", def.ID, err)
		}
	}
	key := string(def.ID) + "\x00" + string(inputSchema) + "\x00" + string(outputSchema)
	if validators, ok := s.validators.Load(key); ok {
		return validators.(*toolValidators), nil
	}
	compiler := lebrojsonschema.NewCompiler()
	inputValidator, err := compiler.Compile(inputSchema)
	if err != nil {
		return nil, fmt.Errorf("lebro/mcp: tool %q: compile input schema: %w", def.ID, err)
	}
	var outputValidator lebro.CompiledSchema
	if len(outputSchema) > 0 {
		outputValidator, err = compiler.Compile(outputSchema)
		if err != nil {
			return nil, fmt.Errorf("lebro/mcp: tool %q: compile output schema: %w", def.ID, err)
		}
	}
	validators := &toolValidators{input: inputValidator, output: outputValidator}
	actual, _ := s.validators.LoadOrStore(key, validators)
	return actual.(*toolValidators), nil
}

func (s *Server) exposeTool(def lebro.ToolDefinition, execute func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult, validators *toolValidators) error {
	inputSchema, err := normalizeInputSchema(def.InputSchema)
	if err != nil {
		return fmt.Errorf("lebro/mcp: tool %q: %w", def.ID, err)
	}
	var outputSchema json.RawMessage
	if len(def.OutputSchema) > 0 {
		outputSchema, err = normalizeOutputSchema(def.OutputSchema)
		if err != nil {
			return fmt.Errorf("lebro/mcp: tool %q: %w", def.ID, err)
		}
	}
	if err := s.registerName(string(def.ID)); err != nil {
		return err
	}
	mcpTool := &mcpsdk.Tool{Name: string(def.ID), Description: def.Description, InputSchema: inputSchema, OutputSchema: outputSchema}
	s.mcpServer.AddTool(mcpTool, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		return toolResultToMCP(executeAdapterTool(ctx, def.ID, arguments, execute, validators))
	})
	return nil
}

func executeAdapterTool(ctx context.Context, id lebro.ToolID, arguments json.RawMessage, execute func(context.Context, lebro.ToolExecutionRequest) lebro.ToolExecutionResult, validators *toolValidators) (result lebro.ToolExecutionResult) {
	if ctx == nil {
		return lebro.ToolExecutionResult{ToolID: id, State: lebro.ToolExecutionHandlerError, Err: errors.New("lebro/mcp: tool context is nil")}
	}
	if err := ctx.Err(); err != nil {
		return lebro.ToolExecutionResult{ToolID: id, State: lebro.ToolExecutionCancelled, Err: err}
	}
	if validators != nil {
		if err := validators.input.Validate(arguments); err != nil {
			return lebro.ToolExecutionResult{ToolID: id, State: lebro.ToolExecutionInvalidInput, Err: err}
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result = lebro.ToolExecutionResult{ToolID: id, State: lebro.ToolExecutionPanicked, Err: &lebro.ToolPanicError{Value: recovered}}
			return
		}
		if err := ctx.Err(); err != nil {
			result = lebro.ToolExecutionResult{ToolID: id, State: lebro.ToolExecutionCancelled, Err: err}
			return
		}
		result.ToolID = id
		if result.State == lebro.ToolExecutionSucceeded && validators != nil && validators.output != nil {
			if err := validators.output.Validate(result.Output); err != nil {
				result = lebro.ToolExecutionResult{ToolID: id, State: lebro.ToolExecutionInvalidOutput, Err: err}
			}
		}
	}()
	return execute(ctx, lebro.ToolExecutionRequest{Arguments: arguments})
}
