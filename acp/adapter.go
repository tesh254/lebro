// Package acp implements the ACP v1 adapter. See doc.go for the package
// contract and docs/acp.md for the SDK evaluation and protocol notes.
package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

// ServerConfig configures an ACP server. AgentConfig must include the Model
// (or Router) and the Definition; a Store enables multi-turn history. The
// server wraps the config's ToolApprovalPolicy with its ACP permission bridge
// (a nil policy means every model-requested tool call requires approval) and
// installs its own event listener, so applications must not set Listener or
// ToolObserver — NewServer rejects a config that does.
type ServerConfig struct {
	// AgentConfig is the configuration used to build the exposed agent. The
	// server clones it and installs its own bridges.
	AgentConfig *lebro.AgentConfig
	// AgentInfo names the agent in the initialize response. An empty name
	// defaults to the agent definition's ID.
	AgentInfo acp.Implementation
	// Logger receives connection diagnostics. Diagnostics never touch the
	// protocol stdout; RunStdio sends them to stderr.
	Logger *slog.Logger
}

// Implementation re-exports the SDK's implementation info so applications
// configure the server without importing the SDK directly.
type Implementation = acp.Implementation

// Server exposes lebro agents over ACP v1.
type Server struct {
	config ServerConfig
	logger *slog.Logger

	mu       sync.Mutex
	exposed  *lebro.Agent
	sessions map[string]*session
	turns    map[string]int
	perms    *permissionBridge
	// serveCtx is the active connection's context, set when serving starts
	// and cancelled when that connection ends.
	serveCtx context.Context

	// connection is set once the server starts serving.
	connection  *acp.AgentSideConnection
	initialized bool
}

// NewServer validates the configuration and returns a ready server.
func NewServer(config ServerConfig) (*Server, error) {
	if config.AgentConfig == nil {
		return nil, errors.New("acp: ServerConfig.AgentConfig is required")
	}
	if config.AgentConfig.Definition.ID == "" {
		return nil, errors.New("acp: ServerConfig.AgentConfig.Definition.ID is required")
	}
	if config.AgentConfig.Store == nil {
		return nil, errors.New("acp: ServerConfig.AgentConfig.Store is required for session history and approvals")
	}
	for name, set := range map[string]bool{
		"Listener":     config.AgentConfig.Listener != nil,
		"ToolObserver": config.AgentConfig.ToolObserver != nil,
	} {
		if set {
			return nil, fmt.Errorf("acp: ServerConfig.AgentConfig.%s must be nil; the server installs its own bridge", name)
		}
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{
		config:   config,
		logger:   logger,
		sessions: make(map[string]*session),
		turns:    make(map[string]int),
	}, nil
}

// ExposeAgent builds the agent from the server's AgentConfig and registers it
// as the served agent. The server installs its own event bridge (listener),
// permission bridge (approval policy), and agent config wrapping, so the
// bridge lifecycle is owned here rather than by application code.
func (s *Server) ExposeAgent() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exposedAgent() {
		return errors.New("acp: an agent is already exposed")
	}
	s.perms = newPermissionBridge(s, s.config.AgentConfig.ToolApprovalPolicy)
	agent, err := s.buildAgentLocked(*s.config.AgentConfig)
	if err != nil {
		return err
	}
	s.exposed = agent
	return nil
}

// buildAgentLocked constructs the agent with the ACP bridges installed. The
// permission bridge wraps the configured approval policy (nil means every
// model-requested call requires ACP approval), and the event bridge is bound
// per turn inside the listener fan-out.
func (s *Server) buildAgentLocked(cfg lebro.AgentConfig) (*lebro.Agent, error) {
	// The turn listener is registered per prompt via the session bridge; the
	// agent-level listener fans out to it.
	cfg.Listener = listenerFunc(func(event lebro.RunEvent) {
		s.mu.Lock()
		bridge := s.activeBridgeForRun(string(event.RunID))
		s.mu.Unlock()
		if bridge == nil {
			return
		}
		bridge.OnRunEvent(event)
	})
	cfg.ToolApprovalPolicy = s.perms
	return lebro.NewAgent(cfg)
}

// permissionBridge returns the server's approval bridge.
func (s *Server) permissionBridge() *permissionBridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.perms
}

// activeSessionForRun returns the session whose running turn owns a run ID.
func (s *Server) activeSessionForRun(runID string) *session {
	for _, sess := range s.sessions {
		if sess.running && sess.bridge != nil && sess.bridge.runID == runID {
			return sess
		}
	}
	return nil
}

// activeBridgeForRun returns the bridge whose turn owns a run ID, or nil.
func (s *Server) activeBridgeForRun(runID string) *eventBridge {
	if sess := s.activeSessionForRun(runID); sess != nil {
		return sess.bridge
	}
	return nil
}

// activeBridge returns the event bridge whose turn owns a run ID, even after
// the prompt goroutine has finished; terminal approval cleanups use it to
// route failed tool-call updates to the right session.
func (s *Server) activeBridge(runID string) *eventBridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeBridgeForRun(runID)
}

func (s *Server) exposedAgent() bool { return s.exposed != nil }

// agent returns the exposed agent.
func (s *Server) agent() *lebro.Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exposed
}

// ServerAgent exposes the served agent for tests in the acp_test package.
func (s *Server) ServerAgent() *lebro.Agent { return s.agent() }

// listenerFunc adapts a function to lebro.RunListener.
type listenerFunc func(lebro.RunEvent)

func (fn listenerFunc) OnRunEvent(event lebro.RunEvent) { fn(event) }

// RunStdio serves ACP v1 over the process stdin and stdout. It blocks until
// the client disconnects. Protocol messages own stdout; server diagnostics go
// to the configured logger.
func (s *Server) RunStdio(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if !s.exposedAgent() {
		s.mu.Unlock()
		return errors.New("acp: no agent exposed; call ExposeAgent first")
	}
	if s.connection != nil {
		s.mu.Unlock()
		return errors.New("acp: server is already serving")
	}
	conn := acp.NewAgentSideConnection(s, os.Stdout, os.Stdin)
	s.connection = conn
	s.mu.Unlock()
	closed := s.watchConnection(conn)

	select {
	case <-conn.Done():
		<-closed
		return nil
	case <-ctx.Done():
		_ = os.Stdin.Close()
		<-conn.Done()
		<-closed
		return ctx.Err()
	}
}

// ServeForTest wires the server to arbitrary reader and writer pipes. It is
// exported for the package's tests and for embedding hosts that own their own
// transport; production agents should use RunStdio. The connection starts
// immediately and Done() closes when the peer disconnects. Protocol output
// goes to output; callers own both pipes' lifecycle. A server serves one
// connection at a time: a second call replaces the live connection, so its
// sessions are dropped and its in-flight turns are cancelled.
func (s *Server) ServeForTest(output io.Writer, input io.Reader) *acp.AgentSideConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connection != nil {
		s.stopServingLocked()
	}
	conn := acp.NewAgentSideConnection(s, output, input)
	s.connection = conn
	s.watchConnection(conn)
	return conn
}

// stopServingLocked cancels the active turns of a connection being replaced.
// The old connection's watcher goroutine clears its own session map when it
// observes the disconnect; cancelling here keeps the new connection from
// inheriting another transport's runs.
func (s *Server) stopServingLocked() {
	for _, sess := range s.sessions {
		if sess.cancel != nil {
			sess.cancel()
		}
	}
}

// Compile-time check that Server implements the SDK agent surface. The
// unsupported optional methods return method-not-found, matching the
// capabilities the server does not advertise.
var _ acp.Agent = (*Server)(nil)

// session is one ACP session: an opaque protocol ID mapped to a lebro thread
// and the active turn, if any.
type session struct {
	id       string
	thread   lebro.ThreadID
	running  bool
	bridge   *eventBridge
	cancel   context.CancelFunc
	turn     int
	agent    *lebro.Agent
	closeMCP func()
}

// SetAgentConnection implements the SDK's connection-aware hook so the server
// can send session updates and permission requests.
func (s *Server) SetAgentConnection(conn *acp.AgentSideConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connection = conn
}

func (s *Server) conn() *acp.AgentSideConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connection
}

func (s *Server) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
	// Negotiate the client's version when we support it; otherwise answer
	// with the latest version we support and let the client disconnect.
	version := params.ProtocolVersion
	if version != acp.ProtocolVersionNumber {
		version = acp.ProtocolVersionNumber
	}
	info := s.config.AgentInfo
	if info.Name == "" {
		info.Name = string(s.config.AgentConfig.Definition.ID)
	}
	if info.Version == "" {
		info.Version = "0.0.0"
	}
	return acp.InitializeResponse{
		ProtocolVersion: version,
		AgentInfo:       &info,
		// Only baseline capabilities: loadSession false, no prompt content
		// beyond text and resource_link, no MCP transports, no auth methods.
		AgentCapabilities: acp.AgentCapabilities{},
		AuthMethods:       []acp.AuthMethod{},
	}, nil
}

func (s *Server) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewMethodNotFound(acp.AgentMethodAuthenticate)
}

func (s *Server) Logout(ctx context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

func (s *Server) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if !initialized {
		return acp.NewSessionResponse{}, acp.NewInvalidRequest(map[string]any{"error": "initialize must complete first"})
	}
	if len(params.AdditionalDirectories) > 0 {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{
			"error": "additionalDirectories are not supported by this agent",
		})
	}
	id, err := randomSessionID()
	if err != nil {
		return acp.NewSessionResponse{}, acp.NewInternalError(map[string]any{"error": err.Error()})
	}
	agent, closeMCP, err := s.sessionAgent(ctx, params)
	if err != nil {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if err := ctx.Err(); err != nil {
		closeMCP()
		return acp.NewSessionResponse{}, err
	}
	sess := &session{id: id, thread: lebro.ThreadID("acp-" + id), agent: agent, closeMCP: closeMCP}
	s.mu.Lock()
	if s.connection == nil {
		s.mu.Unlock()
		closeMCP()
		return acp.NewSessionResponse{}, acp.NewInternalError(map[string]any{"error": "connection unavailable"})
	}
	select {
	case <-s.connection.Done():
		s.mu.Unlock()
		closeMCP()
		return acp.NewSessionResponse{}, acp.NewInternalError(map[string]any{"error": "connection closed"})
	default:
	}
	s.sessions[id] = sess
	s.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(id)}, nil
}

func (s *Server) lookupSession(id acp.SessionId) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[string(id)]
	return sess, ok
}

// loadSession and resume are not advertised; reject them explicitly.
func (s *Server) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	return acp.LoadSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
}

func (s *Server) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (s *Server) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (s *Server) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}

func (s *Server) SetSessionMode(ctx context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func (s *Server) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (s *Server) Cancel(ctx context.Context, params acp.CancelNotification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[string(params.SessionId)]; sess != nil && sess.cancel != nil {
		sess.cancel()
	}
	return nil
}

// sessionByThread finds the session that owns a lebro thread. Used by the
// permission bridge to route a request back to its session.
func (s *Server) sessionByThread(thread lebro.ThreadID) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.thread == thread {
			return sess
		}
	}
	return nil
}

// randomSessionID returns an opaque, unguessable session ID. It never
// resembles a lebro ThreadID so protocol peers cannot select threads by name.
func randomSessionID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("acp: generate session id: %w", err)
	}
	return "sess_" + hex.EncodeToString(buf[:]), nil
}
