package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

// permissionBridge preserves the configured approval policy and defaults to
// requiring approval when no policy is configured.
type permissionBridge struct {
	server  *Server
	wrapped lebro.ToolApprovalPolicy
}

func newPermissionBridge(server *Server, wrapped lebro.ToolApprovalPolicy) *permissionBridge {
	return &permissionBridge{server: server, wrapped: wrapped}
}

func (p *permissionBridge) EvaluateToolApproval(ctx context.Context, invocation lebro.ToolApprovalInvocation) (lebro.ToolApprovalResolution, error) {
	resolution := lebro.ToolApprovalResolution{Outcome: lebro.ToolApprovalRequire}
	// The runtime's policy contract treats a typed-nil wrapper as absent;
	// mirror that here so a typed-nil policy cannot panic ACP runs.
	if p.wrapped != nil && !isNilPolicy(p.wrapped) {
		wrappedResolution, err := p.wrapped.EvaluateToolApproval(ctx, invocation)
		if err != nil {
			return lebro.ToolApprovalResolution{}, err
		}
		// The runtime resolves an empty outcome to allow; preserve that
		// default-allow behavior instead of rejecting the zero value.
		if wrappedResolution.Outcome == "" {
			wrappedResolution.Outcome = lebro.ToolApprovalAllow
		}
		switch wrappedResolution.Outcome {
		case lebro.ToolApprovalAllow:
			return wrappedResolution, nil
		case lebro.ToolApprovalDeny:
			return wrappedResolution, nil
		case lebro.ToolApprovalRequire:
			resolution = wrappedResolution
		default:
			return lebro.ToolApprovalResolution{}, fmt.Errorf("acp: wrapped policy returned invalid outcome %q", wrappedResolution.Outcome)
		}
	}
	return resolution, nil
}

// isNilPolicy reports whether a non-nil interface value carries a nil
// concrete policy, matching the runtime's isNilInterface detection.
func isNilPolicy(policy lebro.ToolApprovalPolicy) bool {
	value := reflect.ValueOf(policy)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// requestPermission presents the persisted request and returns the decision.
func (p *permissionBridge) requestPermission(ctx context.Context, request *lebro.ToolApprovalRequest) (bool, string) {
	conn := p.server.conn()
	sess := p.server.sessionByThread(request.ThreadID)
	if conn == nil || sess == nil {
		return false, "client connection unavailable"
	}
	title := toolTitle(request.ToolID, request.ToolCallID)
	kind := toolKind(request.ToolID)
	if !json.Valid(request.Arguments) {
		return false, "invalid persisted arguments"
	}
	response, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(sess.id),
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(request.ToolCallID),
			Title:      acp.Ptr(title), Kind: acp.Ptr(kind),
			Status: acp.Ptr(acp.ToolCallStatusPending), RawInput: json.RawMessage(request.Arguments),
		},
		Options: []acp.PermissionOption{
			{OptionId: "allow", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		p.server.logger.Debug("acp: permission request failed", "session", sess.id, "error", err)
		return false, "permission request failed"
	}
	if response.Outcome.Selected != nil && response.Outcome.Selected.OptionId == "allow" {
		return true, "approved via ACP"
	}
	return false, "outcome is not approval"
}
