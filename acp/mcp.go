package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/mcp"
)

var mcpServerName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,126}$`)

// sessionAgent creates a private tool registry and MCP clients for one session.
func (s *Server) sessionAgent(ctx context.Context, params acpsdk.NewSessionRequest) (*lebro.Agent, func(), error) {
	if len(params.McpServers) == 0 {
		return s.agent(), func() {}, nil
	}
	cfg := *s.config.AgentConfig
	cfg.Definition.Tools = append([]lebro.ToolID(nil), cfg.Definition.Tools...)
	var err error
	cfg.Tools, err = cfg.Tools.CloneWithCompiler(jsonschema.NewMCPCompiler())
	if err != nil {
		return nil, nil, err
	}
	var clients []*mcp.Client
	cleanup := func() {
		for _, client := range clients {
			_ = client.Close()
		}
	}
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()
	seen := make(map[string]bool)
	for _, server := range params.McpServers {
		spec := server.Stdio
		if spec == nil {
			return nil, nil, fmt.Errorf("only stdio MCP servers are supported")
		}
		if !mcpServerName.MatchString(spec.Name) || seen[spec.Name] {
			return nil, nil, fmt.Errorf("invalid or duplicate MCP server name %q", spec.Name)
		}
		seen[spec.Name] = true
		cmd := exec.Command(spec.Command, spec.Args...)
		cmd.Dir = params.Cwd
		cmd.Env = make([]string, 0, len(spec.Env)+2)
		for _, name := range []string{"PATH", "HOME"} {
			if value, ok := os.LookupEnv(name); ok {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
		}
		for _, env := range spec.Env {
			if env.Name == "" || strings.ContainsAny(env.Name, "=\x00") || strings.ContainsRune(env.Value, '\x00') {
				return nil, nil, fmt.Errorf("invalid MCP environment variable")
			}
			cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
		}
		cmd.Stderr = os.Stderr
		client := mcp.NewClient(mcp.ClientConfig{Implementation: &mcpsdk.Implementation{Name: "lebro-acp", Version: "1.0.0"}, ServerName: spec.Name})
		clients = append(clients, client)
		if err := client.Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}); err != nil {
			return nil, nil, fmt.Errorf("connect MCP server %q: %w", spec.Name, err)
		}
		tools, err := client.DiscoverTools(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("discover MCP server %q: %w", spec.Name, err)
		}
		for _, tool := range tools {
			if err := cfg.Tools.Register(tool); err != nil {
				return nil, nil, err
			}
			cfg.Definition.Tools = append(cfg.Definition.Tools, tool.Definition().ID)
		}
	}
	s.mu.Lock()
	agent, err := s.buildAgentLocked(cfg)
	s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	success = true
	return agent, cleanup, nil
}

// watchConnection closes all session-owned MCP processes after disconnect.
func (s *Server) watchConnection(conn *acpsdk.AgentSideConnection) <-chan struct{} {
	closed := make(chan struct{})
	// Called with s.mu held; read the previous connection context directly.
	parent := s.serveCtx
	if parent == nil {
		parent = context.Background()
	}
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.serveCtx = serveCtx
	go func() {
		defer func() {
			cancel()
			close(closed)
		}()
		<-conn.Done()
		s.mu.Lock()
		sessions := s.sessions
		s.sessions = make(map[string]*session)
		for _, sess := range sessions {
			if sess.cancel != nil {
				sess.cancel()
			}
		}
		s.mu.Unlock()
		for _, sess := range sessions {
			sess.closeMCP()
		}
	}()
	return closed
}

// serveContext returns the active connection's context.
func (s *Server) serveContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveCtx
}
