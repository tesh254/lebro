package mcp

import (
	"errors"
	"fmt"
)

// Discovery and invocation failures are reported through distinct sentinels so
// callers can tell them apart with errors.Is, including from a run record's
// recorded error. Discovery failures happen before a tool is registered, so
// they never reach a run record; invocation failures surface as the wrapped
// cause of a ToolExecutionHandlerError.
var (
	// ErrRemoteDiscovery marks a failure to list tools on a remote MCP server.
	ErrRemoteDiscovery = errors.New("lebro/mcp: remote tool discovery failed")
	// ErrRemoteInvocation marks a transport or protocol failure while calling a
	// remote MCP tool. The call did not produce a tool-level result.
	ErrRemoteInvocation = errors.New("lebro/mcp: remote tool invocation failed")
	// ErrRemoteToolError marks a remote tool that ran and reported a tool-level
	// error (CallToolResult.IsError). The transport succeeded.
	ErrRemoteToolError = errors.New("lebro/mcp: remote tool reported an error")
	// ErrRemoteInputRequired marks a remote tool that asked for further input
	// before it could complete. The call neither succeeded nor failed: the
	// server expects the caller to fulfill its input requests and retry. This
	// package does not implement that exchange, so the call is reported rather
	// than silently treated as an empty result.
	ErrRemoteInputRequired = errors.New("lebro/mcp: remote tool requires further input")
)

// RemoteToolError associates a remote failure with the server and tool that
// produced it. Its Unwrap chain reaches the sentinel describing the failure
// class, so errors.Is(err, ErrRemoteToolError) and errors.As(err,
// &RemoteToolError{}) both work on an error retrieved from a run record.
type RemoteToolError struct {
	// ServerName is the configured name of the remote server.
	ServerName string
	// ToolName is the tool's name as advertised by the remote server, without
	// the local ID prefix.
	ToolName string
	// Message is the remote error text when the server supplied one.
	Message string
	// Err is the failure class sentinel, or the underlying transport error.
	Err error
}

func (e *RemoteToolError) Error() string {
	if e == nil {
		return ""
	}
	base := fmt.Sprintf("lebro/mcp: server %q tool %q", e.ServerName, e.ToolName)
	switch {
	case e.Message != "" && e.Err != nil:
		return fmt.Sprintf("%s: %v: %s", base, e.Err, e.Message)
	case e.Message != "":
		return base + ": " + e.Message
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", base, e.Err)
	default:
		return base
	}
}

// Unwrap exposes the failure class sentinel or underlying transport error.
func (e *RemoteToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// RemoteDiscoveryError associates a discovery failure with the server that
// produced it. It always unwraps to ErrRemoteDiscovery.
type RemoteDiscoveryError struct {
	// ServerName is the configured name of the remote server.
	ServerName string
	// Err is the underlying transport or protocol error.
	Err error
}

func (e *RemoteDiscoveryError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro/mcp: server %q: %v", e.ServerName, ErrRemoteDiscovery)
	}
	return fmt.Sprintf("lebro/mcp: server %q: %v: %v", e.ServerName, ErrRemoteDiscovery, e.Err)
}

// Unwrap reports both ErrRemoteDiscovery and the underlying cause so callers
// can match either the failure class or a specific transport error.
func (e *RemoteDiscoveryError) Unwrap() []error {
	if e == nil {
		return nil
	}
	if e.Err == nil {
		return []error{ErrRemoteDiscovery}
	}
	return []error{ErrRemoteDiscovery, e.Err}
}
