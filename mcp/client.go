package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
)

// ClientConfig configures a client that consumes tools from a remote MCP
// server.
type ClientConfig struct {
	// Implementation identifies this client to the remote server. Required.
	Implementation *mcpsdk.Implementation
	// ServerName namespaces every discovered tool's local ID as
	// "<ServerName>.<remote name>". Two servers that both advertise a "search"
	// tool therefore stay distinguishable in one registry. Required, and
	// restricted to the MCP tool name character set so the resulting IDs remain
	// valid if they are re-exposed through a Server.
	ServerName string
	// Options are passed through to the underlying SDK client. Optional.
	Options *mcpsdk.ClientOptions
}

// Client connects to a remote MCP server and adapts its tools to the lebro
// Tool contract. The zero value is not usable; construct one with NewClient.
type Client struct {
	config    ClientConfig
	sdkClient *mcpsdk.Client

	mu      sync.Mutex
	session *mcpsdk.ClientSession
}

// NewClient creates a client for a remote MCP server. It panics when required
// configuration is missing, matching NewServer, because a misconfigured client
// cannot do anything useful and the mistake is always a programming error.
func NewClient(config ClientConfig) *Client {
	if config.Implementation == nil {
		panic("lebro/mcp: Implementation is required")
	}
	if config.ServerName == "" {
		panic("lebro/mcp: ServerName is required")
	}
	if !mcpToolNamePattern.MatchString(config.ServerName) {
		panic(fmt.Errorf("lebro/mcp: ServerName %q %s", config.ServerName, toolNameRequirement))
	}
	// A name long enough to leave no room for the separator and a remote tool
	// name would pass the pattern above and then fail on every tool, making a
	// constructible client that can never discover anything. Rejecting it here
	// puts the error where the mistake is.
	if len(config.ServerName) > maxServerNameLength {
		panic(fmt.Errorf("lebro/mcp: ServerName %q is %d chars; the limit is %d so namespaced tool IDs stay within the MCP limit of %d", config.ServerName, len(config.ServerName), maxServerNameLength, maxToolNameLength))
	}
	return &Client{
		config:    config,
		sdkClient: mcpsdk.NewClient(config.Implementation, config.Options),
	}
}

// Connect establishes a session with the remote server over transport. The
// caller owns the session lifetime and should call Close when finished.
func (c *Client) Connect(ctx context.Context, transport mcpsdk.Transport) error {
	if c == nil {
		return errors.New("lebro/mcp: client is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return errors.New("lebro/mcp: client is already connected")
	}
	session, err := c.sdkClient.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("lebro/mcp: connect to server %q: %w", c.config.ServerName, err)
	}
	c.session = session
	return nil
}

// Close ends the session with the remote server. Closing a client that was
// never connected is a no-op so cleanup paths can call it unconditionally.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	session := c.session
	c.session = nil
	c.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

// Session exposes the underlying SDK session for capabilities this package
// does not wrap, such as prompts and resources. It returns nil before Connect.
func (c *Client) Session() *mcpsdk.ClientSession {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// DiscoverTools lists every tool the remote server advertises and adapts each
// one to the lebro Tool contract. Pagination is followed to completion so
// callers see the whole tool set rather than the first page.
//
// The returned tools are ordered as the server returned them and are ready to
// pass to ToolRegistry.Register, which compiles their schemas and produces the
// validated execution boundary. Failures wrap ErrRemoteDiscovery.
func (c *Client) DiscoverTools(ctx context.Context) ([]lebro.Tool, error) {
	if c == nil {
		return nil, &RemoteDiscoveryError{Err: errors.New("lebro/mcp: client is nil")}
	}
	session := c.Session()
	if session == nil {
		return nil, &RemoteDiscoveryError{
			ServerName: c.config.ServerName,
			Err:        errors.New("lebro/mcp: client is not connected"),
		}
	}

	var (
		tools  []lebro.Tool
		cursor string
		seen   = make(map[string]struct{})
	)
	for {
		result, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, &RemoteDiscoveryError{ServerName: c.config.ServerName, Err: err}
		}
		for _, remote := range result.Tools {
			if remote == nil {
				continue
			}
			adapted, err := c.adaptTool(remote)
			if err != nil {
				return nil, &RemoteDiscoveryError{ServerName: c.config.ServerName, Err: err}
			}
			tools = append(tools, adapted)
		}
		if result.NextCursor == "" {
			break
		}
		// A server that repeats a cursor would otherwise spin here forever.
		if _, repeated := seen[result.NextCursor]; repeated {
			return nil, &RemoteDiscoveryError{
				ServerName: c.config.ServerName,
				Err:        fmt.Errorf("lebro/mcp: server repeated pagination cursor %q", result.NextCursor),
			}
		}
		seen[result.NextCursor] = struct{}{}
		cursor = result.NextCursor
	}
	return tools, nil
}

// adaptTool converts one remote tool description into a lebro Tool bound to
// this client's session.
func (c *Client) adaptTool(remote *mcpsdk.Tool) (lebro.Tool, error) {
	name := strings.TrimSpace(remote.Name)
	if name == "" {
		return nil, errors.New("lebro/mcp: remote tool name is empty")
	}

	inputSchema, err := remoteSchemaToRaw(remote.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("lebro/mcp: tool %q input schema: %w", name, err)
	}
	// A remote server may omit the input schema or describe a non-object; the
	// lebro boundary validates arguments as an object, so normalize the same
	// way the server half does before exposing a tool.
	normalizedInput, err := normalizeInputSchema(inputSchema)
	if err != nil {
		return nil, fmt.Errorf("lebro/mcp: tool %q input schema: %w", name, err)
	}

	// Output schemas are advisory in MCP and many servers omit them. When one is
	// advertised it is carried through, so the local boundary validates results
	// against the server's own contract. When none is advertised the adapter
	// still publishes a schema — the fixed text envelope it produces in that
	// case — because a tool whose output shape is undeclared cannot be
	// validated or relied on by callers.
	var (
		outputSchema   json.RawMessage
		remoteDeclared bool
	)
	if remote.OutputSchema != nil {
		raw, err := remoteSchemaToRaw(remote.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("lebro/mcp: tool %q output schema: %w", name, err)
		}
		normalizedOutput, err := normalizeOutputSchema(raw)
		if err != nil {
			return nil, fmt.Errorf("lebro/mcp: tool %q output schema: %w", name, err)
		}
		outputSchema = normalizedOutput
		remoteDeclared = len(outputSchema) > 0
	}
	if !remoteDeclared {
		outputSchema = json.RawMessage(textEnvelopeSchema)
	}

	id := c.config.ServerName + "." + name
	if !mcpToolNamePattern.MatchString(id) {
		return nil, fmt.Errorf("lebro/mcp: tool ID %q %s", id, toolNameRequirement)
	}

	return &remoteTool{
		client:         c,
		serverName:     c.config.ServerName,
		remoteName:     name,
		remoteDeclared: remoteDeclared,
		definition: lebro.ToolDefinition{
			ID:           lebro.ToolID(id),
			Description:  remote.Description,
			InputSchema:  append([]byte(nil), normalizedInput...),
			OutputSchema: append([]byte(nil), outputSchema...),
		},
	}, nil
}
