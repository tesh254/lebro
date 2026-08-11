package mcp

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpToolNamePattern matches the MCP spec's tool name character set: letters,
// digits, underscores, hyphens, and dots, up to 128 characters.
var mcpToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// ServerConfig configures an MCP server that exposes lebro primitives.
type ServerConfig struct {
	// Implementation identifies the MCP server to clients. Required.
	Implementation *mcpsdk.Implementation
	// Instructions is optional server-level guidance sent to clients.
	Instructions string
	// PageSize is the maximum number of items returned in a single list
	// response. Zero uses the SDK default (1000).
	PageSize int
}

// Server exposes selected lebro tools, agents, and workflows through an MCP
// server. Only explicitly registered primitives are visible to MCP clients.
// The zero value is not usable; construct one with NewServer.
type Server struct {
	mcpServer *mcpsdk.Server
	mu        sync.Mutex
	exposed   map[string]struct{}
}

// NewServer creates an MCP server that exposes lebro primitives. The server
// has no features until ExposeTool, ExposeAgent, or ExposeWorkflow is called.
func NewServer(config ServerConfig) *Server {
	if config.Implementation == nil {
		panic("lebro/mcp: Implementation is required")
	}
	opts := &mcpsdk.ServerOptions{
		Instructions: config.Instructions,
		Capabilities: &mcpsdk.ServerCapabilities{
			Tools: &mcpsdk.ToolCapabilities{ListChanged: true},
		},
	}
	if config.PageSize < 0 {
		panic(fmt.Errorf("lebro/mcp: PageSize must not be negative, got %d", config.PageSize))
	}
	if config.PageSize > 0 {
		opts.PageSize = config.PageSize
	}
	return &Server{
		mcpServer: mcpsdk.NewServer(config.Implementation, opts),
		exposed:   make(map[string]struct{}),
	}
}

func toolError(message string) *mcpsdk.CallToolResult {
	result := &mcpsdk.CallToolResult{}
	result.SetError(fmt.Errorf("lebro/mcp: %s", message))
	return result
}

// Connect connects the MCP server over the given transport and returns a
// server session. This is the lower-level alternative to Run for servers that
// need to manage sessions explicitly (for example, multi-session deployments
// or in-memory testing). The caller is responsible for closing the session.
func (s *Server) Connect(ctx context.Context, transport mcpsdk.Transport) (*mcpsdk.ServerSession, error) {
	return s.mcpServer.Connect(ctx, transport, nil)
}

// Run runs the MCP server over the given transport until the client disconnects
// or the context is cancelled. This is a convenience for servers that handle a
// single session; for multi-session deployments use MCPServer and the
// StreamableHTTPHandler from the SDK.
func (s *Server) Run(ctx context.Context, transport mcpsdk.Transport) error {
	return s.mcpServer.Run(ctx, transport)
}

// StreamableHTTPHandler returns an HTTP handler for multi-session MCP
// deployments. The handler serves this Server's explicit allow-list over the
// SDK's Streamable HTTP transport.
func (s *Server) StreamableHTTPHandler(opts *mcpsdk.StreamableHTTPOptions) http.Handler {
	if opts == nil {
		opts = &mcpsdk.StreamableHTTPOptions{
			Stateless:                    true,
			PropagateRequestCancellation: true,
		}
	}
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return s.mcpServer
	}, opts)
}

// registerName reserves a tool name in the allow-list. It returns an error if
// the name is empty, already exposed, or does not conform to the MCP tool name
// character set (letters, digits, underscores, hyphens, dots; max 128 chars).
func (s *Server) registerName(name string) error {
	if name == "" {
		return fmt.Errorf("lebro/mcp: tool name is required")
	}
	if !mcpToolNamePattern.MatchString(name) {
		return fmt.Errorf("lebro/mcp: tool name %q must contain only letters, digits, underscores, hyphens, and dots (max 128 chars)", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.exposed[name]; exists {
		return fmt.Errorf("lebro/mcp: %q is already exposed", name)
	}
	s.exposed[name] = struct{}{}
	return nil
}
