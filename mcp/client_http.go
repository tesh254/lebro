package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConnectionMode describes the lifecycle selected while connecting to a
// Streamable HTTP server.
type ConnectionMode string

const (
	// ConnectionModeUnknown is reported before a Streamable HTTP connection has
	// completed its handshake.
	ConnectionModeUnknown ConnectionMode = "unknown"
	// ConnectionModeStateless is a sessionless connection, including modern
	// 2026-07-28 server/discover connections.
	ConnectionModeStateless ConnectionMode = "stateless"
	// ConnectionModeStateful is a legacy initialize connection for which the
	// server assigned a session. Related operations reuse that session until it
	// is closed, expires, or is lost.
	ConnectionModeStateful ConnectionMode = "stateful"
)

// ConnectionHealth is a safe snapshot of a Streamable HTTP connection. It
// deliberately reports only whether a session exists: session identifiers stay
// inside the SDK transport and must not be used as application identity.
type ConnectionHealth struct {
	ProtocolVersion string
	Mode            ConnectionMode
	HasSession      bool
	Connected       bool
	Closed          bool
	SessionLost     bool
	ExpiresAt       time.Time
}

// StreamableHTTPConfig configures ConnectStreamableHTTP. Applications own
// authentication, tenant/principal partitioning, and one Client per isolated
// connection; this package never derives identity from an MCP session ID.
type StreamableHTTPConfig struct {
	Endpoint             string
	HTTPClient           *http.Client
	MaxRetries           int
	DisableStandaloneSSE bool
	// IdleTimeout bounds a retained legacy session. A zero value leaves its
	// lifetime entirely under the caller's explicit Close.
	IdleTimeout time.Duration
}

type streamableHTTPState struct {
	config    StreamableHTTPConfig
	health    ConnectionHealth
	idleTimer *time.Timer
}

// ErrRemoteSessionLost marks a legacy HTTP session that the server has
// forgotten (normally its 404 response). The operation was not retried because
// replaying it could duplicate side effects. Call Reconnect before starting a
// new, independently safe sequence.
var ErrRemoteSessionLost = errors.New("lebro/mcp: remote session lost")

// RemoteSessionLostError includes the underlying SDK transport failure while
// preserving a stable error class for reconnect decisions.
type RemoteSessionLostError struct{ Err error }

func (e *RemoteSessionLostError) Error() string {
	if e == nil || e.Err == nil {
		return ErrRemoteSessionLost.Error()
	}
	return fmt.Sprintf("%v: %v", ErrRemoteSessionLost, e.Err)
}

func (e *RemoteSessionLostError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrRemoteSessionLost}
	}
	return []error{ErrRemoteSessionLost, e.Err}
}

// ConnectStreamableHTTP connects using the SDK's Streamable HTTP transport.
// The SDK probes modern server/discover first and falls back to initialize for
// older servers. It retains the resulting legacy session only on this Client;
// callers should Close it when the related sequence is complete.
func (c *Client) ConnectStreamableHTTP(ctx context.Context, config StreamableHTTPConfig) error {
	if c == nil {
		return errors.New("lebro/mcp: client is nil")
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return errors.New("lebro/mcp: StreamableHTTPConfig.Endpoint is required")
	}
	if config.IdleTimeout < 0 {
		return errors.New("lebro/mcp: StreamableHTTPConfig.IdleTimeout must not be negative")
	}

	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             config.Endpoint,
		HTTPClient:           config.HTTPClient,
		MaxRetries:           config.MaxRetries,
		DisableStandaloneSSE: config.DisableStandaloneSSE,
	}
	if err := c.Connect(ctx, transport); err != nil {
		return err
	}

	session := c.Session()
	health := ConnectionHealth{Mode: ConnectionModeUnknown, Connected: true}
	if initialized := session.InitializeResult(); initialized != nil {
		health.ProtocolVersion = initialized.ProtocolVersion
	}
	health.HasSession = session.ID() != ""
	if health.HasSession && health.ProtocolVersion < "2026-07-28" {
		health.Mode = ConnectionModeStateful
	} else {
		health.Mode = ConnectionModeStateless
	}

	c.mu.Lock()
	c.streamable = &streamableHTTPState{config: config, health: health}
	c.resetIdleTimerLocked()
	c.mu.Unlock()
	return nil
}

// ConnectionHealth returns the current Streamable HTTP health snapshot. It is
// the zero/unknown snapshot for clients connected through another transport.
func (c *Client) ConnectionHealth() ConnectionHealth {
	if c == nil {
		return ConnectionHealth{Mode: ConnectionModeUnknown}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streamable == nil {
		return ConnectionHealth{Mode: ConnectionModeUnknown}
	}
	return c.streamable.health
}

// Reconnect establishes a fresh Streamable HTTP session using the original
// configuration. It never retries or replays the operation that lost a
// session, preserving callers' side-effect and identity boundaries.
func (c *Client) Reconnect(ctx context.Context) error {
	if c == nil {
		return errors.New("lebro/mcp: client is nil")
	}
	c.mu.Lock()
	if c.streamable == nil {
		c.mu.Unlock()
		return errors.New("lebro/mcp: client has no Streamable HTTP configuration")
	}
	config := c.streamable.config
	c.mu.Unlock()
	if err := c.Close(); err != nil {
		return fmt.Errorf("lebro/mcp: close previous Streamable HTTP connection: %w", err)
	}
	return c.ConnectStreamableHTTP(ctx, config)
}

func (c *Client) touchStreamable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIdleTimerLocked()
}

func (c *Client) resetIdleTimerLocked() {
	if c.streamable == nil || c.streamable.config.IdleTimeout == 0 || !c.streamable.health.Connected {
		return
	}
	c.streamable.health.ExpiresAt = time.Now().Add(c.streamable.config.IdleTimeout)
	if c.streamable.idleTimer != nil {
		c.streamable.idleTimer.Stop()
	}
	c.streamable.idleTimer = time.AfterFunc(c.streamable.config.IdleTimeout, func() {
		c.mu.Lock()
		if c.streamable == nil || !c.streamable.health.Connected || time.Until(c.streamable.health.ExpiresAt) > 0 {
			c.mu.Unlock()
			return
		}
		c.streamable.health.Connected = false
		c.streamable.health.Closed = true
		c.mu.Unlock()
		_ = c.Close()
	})
}

func (c *Client) classifyStreamableError(err error) error {
	if err == nil || !errors.Is(err, mcpsdk.ErrSessionMissing) {
		return err
	}
	c.mu.Lock()
	if c.streamable != nil {
		c.streamable.health.Connected = false
		c.streamable.health.SessionLost = true
	}
	c.mu.Unlock()
	return &RemoteSessionLostError{Err: err}
}
