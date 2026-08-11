package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
)

// remoteTool adapts a tool advertised by a remote MCP server to the lebro Tool
// contract. Registering one in a ToolRegistry gives remote tools the same
// schema-checked execution boundary as local ones: arguments are validated
// before they reach the wire, and results are validated on the way back when
// the server advertised an output schema.
type remoteTool struct {
	client     *Client
	serverName string
	remoteName string
	// remoteDeclared records whether the server advertised its own output
	// schema. When it did not, the definition carries textEnvelopeSchema, so
	// the definition alone cannot tell the two cases apart.
	remoteDeclared bool
	definition     lebro.ToolDefinition
}

// Definition returns a caller-owned copy of the adapted definition.
func (t *remoteTool) Definition() lebro.ToolDefinition {
	definition := t.definition
	definition.InputSchema = append(json.RawMessage(nil), t.definition.InputSchema...)
	definition.OutputSchema = append(json.RawMessage(nil), t.definition.OutputSchema...)
	return definition
}

// Execute forwards validated arguments to the remote server and normalizes the
// response.
//
// The three failure modes stay distinguishable by design. Cancellation returns
// the context error unwrapped, so RegisteredTool classifies it as
// ToolExecutionCancelled rather than a handler failure. A transport or protocol
// failure wraps ErrRemoteInvocation: the call never produced a tool result. A
// tool that ran and reported failure wraps ErrRemoteToolError. Both of the
// latter land in the run record as ToolExecutionHandlerError, where errors.Is
// separates them.
func (t *remoteTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	session := t.client.Session()
	if session == nil {
		return nil, &RemoteToolError{
			ServerName: t.serverName,
			ToolName:   t.remoteName,
			Message:    "client is not connected",
			Err:        ErrRemoteInvocation,
		}
	}

	arguments := json.RawMessage(input)
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}

	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      t.remoteName,
		Arguments: arguments,
	})
	if err != nil {
		// Surface cancellation as the bare context error so the execution
		// boundary reports "cancelled" instead of burying it in a handler
		// error. Callers distinguishing a cancelled run from a broken server
		// depend on that distinction.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, &RemoteToolError{
			ServerName: t.serverName,
			ToolName:   t.remoteName,
			Err:        fmt.Errorf("%w: %w", ErrRemoteInvocation, err),
		}
	}
	if result == nil {
		return nil, &RemoteToolError{
			ServerName: t.serverName,
			ToolName:   t.remoteName,
			Message:    "server returned no result",
			Err:        ErrRemoteInvocation,
		}
	}

	if result.IsError {
		message, _ := textFromContent(result.Content)
		return nil, &RemoteToolError{
			ServerName: t.serverName,
			ToolName:   t.remoteName,
			Message:    message,
			Err:        ErrRemoteToolError,
		}
	}

	return t.resultToRaw(result)
}

// resultToRaw converts a successful call into the single output shape this
// tool's definition promises.
//
// Which shape applies is decided by the tool definition, not by the response,
// so a given tool always returns the same shape. Deciding per response — for
// example returning text verbatim whenever it happens to parse as JSON — would
// let one tool alternate between incompatible shapes across calls and make its
// advertised output schema meaningless.
//
// A tool that advertised an output schema returns the payload that schema
// describes: StructuredContent when present, otherwise the text content parsed
// as JSON. A tool that advertised no schema always returns the text envelope
// described by textEnvelopeSchema, because there is no contract that would
// justify passing an arbitrary remote payload through unlabeled.
func (t *remoteTool) resultToRaw(result *mcpsdk.CallToolResult) (json.RawMessage, error) {
	text, skipped := textFromContent(result.Content)

	if !t.remoteDeclared {
		return t.encodeTextEnvelope(text, skipped)
	}

	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, t.invocationError("encode structured content", err)
		}
		return encoded, nil
	}

	// The server promised a structured result and sent only text. Text that
	// parses as JSON is that result; anything else is a broken promise, and
	// reporting it as an invocation failure is more useful than letting output
	// validation report a confusing schema mismatch.
	if text == "" {
		return nil, t.invocationError("server advertised an output schema but returned no content", nil)
	}
	if !json.Valid([]byte(text)) {
		return nil, t.invocationError("server advertised an output schema but returned non-JSON text content", nil)
	}
	return json.RawMessage(text), nil
}

// textEnvelopeSchema describes the output of a remote tool that advertised no
// output schema of its own. Adapting to a fixed envelope keeps such a tool's
// output shape stable across calls.
const textEnvelopeSchema = `{
	"type":"object",
	"required":["text"],
	"properties":{
		"text":{"type":"string"},
		"skipped_content_types":{"type":"array","items":{"type":"string"}}
	},
	"additionalProperties":false
}`

// encodeTextEnvelope renders text content as the fixed envelope. Content types
// with no textual form are named rather than dropped silently, so a caller
// receiving an empty text can tell "the tool said nothing" apart from "the tool
// answered with an image".
func (t *remoteTool) encodeTextEnvelope(text string, skipped []string) (json.RawMessage, error) {
	envelope := struct {
		Text                string   `json:"text"`
		SkippedContentTypes []string `json:"skipped_content_types,omitempty"`
	}{Text: text, SkippedContentTypes: skipped}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, t.invocationError("encode text content", err)
	}
	return encoded, nil
}

func (t *remoteTool) invocationError(message string, err error) error {
	wrapped := ErrRemoteInvocation
	if err != nil {
		wrapped = fmt.Errorf("%w: %w", ErrRemoteInvocation, err)
	}
	return &RemoteToolError{
		ServerName: t.serverName,
		ToolName:   t.remoteName,
		Message:    message,
		Err:        wrapped,
	}
}

// textFromContent concatenates the text parts of an MCP content list and names
// the content types it could not represent as text, in order and without
// duplicates.
func textFromContent(content []mcpsdk.Content) (text string, skipped []string) {
	var (
		builder strings.Builder
		seen    = make(map[string]struct{})
	)
	for _, item := range content {
		if textContent, ok := item.(*mcpsdk.TextContent); ok {
			if builder.Len() > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString(textContent.Text)
			continue
		}
		name := contentTypeName(item)
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		skipped = append(skipped, name)
	}
	return builder.String(), skipped
}

// contentTypeName labels a non-text content item using the MCP content type
// names clients recognize.
func contentTypeName(item mcpsdk.Content) string {
	switch item.(type) {
	case *mcpsdk.ImageContent:
		return "image"
	case *mcpsdk.AudioContent:
		return "audio"
	case *mcpsdk.ResourceLink:
		return "resource_link"
	case *mcpsdk.EmbeddedResource:
		return "resource"
	case nil:
		return "unknown"
	default:
		return fmt.Sprintf("%T", item)
	}
}

// remoteSchemaToRaw re-encodes a schema received from a server. The SDK decodes
// schemas into map[string]any on the client side, so this returns them to the
// raw JSON form the lebro Tool contract uses.
func remoteSchemaToRaw(schema any) (json.RawMessage, error) {
	if schema == nil {
		return nil, nil
	}
	if raw, ok := schema.(json.RawMessage); ok {
		return raw, nil
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}
	return encoded, nil
}
