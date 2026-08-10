package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestModelRouter_PrimaryRouting(t *testing.T) {
	reg := NewProviderRegistry()
	primary := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "primary"}, FinishReason: FinishReasonStop}}}}
	secondary := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "secondary"}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "primary", Model: primary}); err != nil {
		t.Fatalf("register primary: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "secondary", Model: secondary}); err != nil {
		t.Fatalf("register secondary: %v", err)
	}

	router, err := NewModelRouter(ModelRouterConfig{
		Registry: reg,
		Policy:   RoutingPolicy{Primary: "primary"},
	})
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}

	resp, err := router.Generate(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "primary" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "primary")
	}
	if primary.calls != 1 {
		t.Errorf("primary calls = %d, want 1", primary.calls)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary calls = %d, want 0", secondary.calls)
	}
}

func TestModelRouter_PredicateRouting(t *testing.T) {
	reg := NewProviderRegistry()
	modelA := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "A"}, FinishReason: FinishReasonStop}}}}
	modelB := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "B"}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "a", Model: modelA}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "b", Model: modelB}); err != nil {
		t.Fatalf("register b: %v", err)
	}

	router, err := NewModelRouter(ModelRouterConfig{
		Registry: reg,
		Policy: RoutingPolicy{
			Predicate: func(req ModelRequest) ProviderID {
				if req.Model == "route-b" {
					return "b"
				}
				return "a"
			},
		},
	})
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}

	resp, err := router.Generate(context.Background(), ModelRequest{Model: "route-b"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "B" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "B")
	}
}

func TestModelRouter_CapabilityFiltering(t *testing.T) {
	reg := NewProviderRegistry()
	noTools := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}
	withTools := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "no-tools", Model: noTools, Capabilities: ProviderCapabilities{}}); err != nil {
		t.Fatalf("register no-tools: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "with-tools", Model: withTools, Capabilities: ProviderCapabilities{SupportsTools: true}}); err != nil {
		t.Fatalf("register with-tools: %v", err)
	}

	router, err := NewModelRouter(ModelRouterConfig{
		Registry: reg,
		Policy:   RoutingPolicy{Primary: "no-tools"},
	})
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}

	req := ModelRequest{Tools: []ToolDefinition{{ID: "test"}}}
	_, err = router.Generate(context.Background(), req)
	if err == nil {
		t.Fatal("expected capability error")
	}
}

func TestModelRouter_FallbackOnRetryableError(t *testing.T) {
	reg := NewProviderRegistry()
	failModel := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorUnavailable, Message: "down"}}}}
	okModel := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "fallback-ok"}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "primary", Model: failModel}); err != nil {
		t.Fatalf("register primary: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "fallback", Model: okModel}); err != nil {
		t.Fatalf("register fallback: %v", err)
	}

	router, err := NewModelRouter(ModelRouterConfig{
		Registry: reg,
		Policy:   RoutingPolicy{Primary: "primary"},
		Fallback: &FallbackPolicy{Chain: []ProviderID{"fallback"}},
	})
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}

	resp, err := router.Generate(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "fallback-ok" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "fallback-ok")
	}
}

func TestModelRouter_NoFallbackOnNonRetryableError(t *testing.T) {
	reg := NewProviderRegistry()
	failModel := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorAuthentication, Message: "bad key"}}}}
	okModel := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "primary", Model: failModel}); err != nil {
		t.Fatalf("register primary: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "fallback", Model: okModel}); err != nil {
		t.Fatalf("register fallback: %v", err)
	}

	router, err := NewModelRouter(ModelRouterConfig{
		Registry: reg,
		Policy:   RoutingPolicy{Primary: "primary"},
		Fallback: &FallbackPolicy{Chain: []ProviderID{"fallback"}},
	})
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}

	_, err = router.Generate(context.Background(), ModelRequest{})
	if err == nil {
		t.Fatal("expected error for non-retryable failure")
	}
	if okModel.calls != 0 {
		t.Errorf("fallback calls = %d, want 0 (should not have tried fallback)", okModel.calls)
	}
}

func TestModelRouter_EmptyRegistryError(t *testing.T) {
	reg := NewProviderRegistry()
	_, err := NewModelRouter(ModelRouterConfig{Registry: reg})
	if err == nil {
		t.Fatal("expected error for empty registry")
	}
}

func TestModelRouter_NilRegistryError(t *testing.T) {
	_, err := NewModelRouter(ModelRouterConfig{})
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestModelRouter_Accessors(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}
	if err := reg.Register(ProviderEntry{ID: "test", Model: model}); err != nil {
		t.Fatalf("register: %v", err)
	}

	fb := &FallbackPolicy{Chain: []ProviderID{"test"}}
	router, err := NewModelRouter(ModelRouterConfig{
		Registry: reg,
		Policy:   RoutingPolicy{Primary: "test"},
		Fallback: fb,
	})
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}

	if router.Registry() != reg {
		t.Error("Registry() mismatch")
	}
	if router.Policy().Primary != "test" {
		t.Errorf("Policy().Primary = %q, want %q", router.Policy().Primary, "test")
	}
	if router.Fallback() != fb {
		t.Error("Fallback() mismatch")
	}
}

func TestModelRouter_NilRouter(t *testing.T) {
	var router *ModelRouter
	_, err := router.Generate(context.Background(), ModelRequest{})
	if err == nil {
		t.Fatal("expected error from nil router")
	}
	if router.Registry() != nil {
		t.Error("nil router Registry() should be nil")
	}
	if router.Fallback() != nil {
		t.Error("nil router Fallback() should be nil")
	}
}

// --- Fallback tests ---

func TestFallbackPolicy_WalksChain(t *testing.T) {
	reg := NewProviderRegistry()
	fail1 := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorRateLimited}}}}
	fail2 := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorTimeout}}}}
	ok := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "ok"}, FinishReason: FinishReasonStop}}}}

	for _, e := range []ProviderEntry{
		{ID: "a", Model: fail1},
		{ID: "b", Model: fail2},
		{ID: "c", Model: ok},
	} {
		if err := reg.Register(e); err != nil {
			t.Fatalf("register %q: %v", e.ID, err)
		}
	}

	fb := &FallbackPolicy{Chain: []ProviderID{"a", "b", "c"}}
	resp, err := fb.Generate(context.Background(), ModelRequest{}, "primary", reg, ProviderCapabilities{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "ok")
	}
}

func TestFallbackPolicy_SkipsMissingProviders(t *testing.T) {
	reg := NewProviderRegistry()
	ok := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "found"}, FinishReason: FinishReasonStop}}}}
	if err := reg.Register(ProviderEntry{ID: "exists", Model: ok}); err != nil {
		t.Fatalf("register: %v", err)
	}

	fb := &FallbackPolicy{Chain: []ProviderID{"missing", "exists"}}
	resp, err := fb.Generate(context.Background(), ModelRequest{}, "primary", reg, ProviderCapabilities{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "found" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "found")
	}
}

func TestFallbackPolicy_SkipsInsufficientCapabilities(t *testing.T) {
	reg := NewProviderRegistry()
	noTools := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}
	withTools := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "tools-ok"}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "no-tools", Model: noTools}); err != nil {
		t.Fatalf("register no-tools: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "with-tools", Model: withTools, Capabilities: ProviderCapabilities{SupportsTools: true}}); err != nil {
		t.Fatalf("register with-tools: %v", err)
	}

	fb := &FallbackPolicy{Chain: []ProviderID{"no-tools", "with-tools"}}
	reqs := ProviderCapabilities{SupportsTools: true}
	resp, err := fb.Generate(context.Background(), ModelRequest{}, "primary", reg, reqs)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "tools-ok" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "tools-ok")
	}
}

func TestFallbackPolicy_ChainExhausted(t *testing.T) {
	reg := NewProviderRegistry()
	fail := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorUnavailable}}}}
	if err := reg.Register(ProviderEntry{ID: "fail", Model: fail}); err != nil {
		t.Fatalf("register: %v", err)
	}

	fb := &FallbackPolicy{Chain: []ProviderID{"fail"}}
	_, err := fb.Generate(context.Background(), ModelRequest{}, "primary", reg, ProviderCapabilities{})
	if err == nil {
		t.Fatal("expected error when chain exhausted")
	}
}

func TestFallbackPolicy_NonRetryableStopsWalk(t *testing.T) {
	reg := NewProviderRegistry()
	authFail := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorAuthentication}}}}
	ok := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "auth-fail", Model: authFail}); err != nil {
		t.Fatalf("register auth-fail: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "ok", Model: ok}); err != nil {
		t.Fatalf("register ok: %v", err)
	}

	fb := &FallbackPolicy{Chain: []ProviderID{"auth-fail", "ok"}}
	_, err := fb.Generate(context.Background(), ModelRequest{}, "primary", reg, ProviderCapabilities{})
	if err == nil {
		t.Fatal("expected error for non-retryable")
	}
	if ok.calls != 0 {
		t.Errorf("ok calls = %d, want 0 (should stop at non-retryable)", ok.calls)
	}
}

func TestFallbackPolicy_CustomRetryable(t *testing.T) {
	reg := NewProviderRegistry()
	customErr := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorInvalidRequest}}}}
	ok := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant, Content: "custom"}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{ID: "custom", Model: customErr}); err != nil {
		t.Fatalf("register custom: %v", err)
	}
	if err := reg.Register(ProviderEntry{ID: "ok", Model: ok}); err != nil {
		t.Fatalf("register ok: %v", err)
	}

	fb := &FallbackPolicy{
		Chain: []ProviderID{"custom", "ok"},
		Retryable: func(err *ModelError) bool {
			return err.Kind == ModelErrorInvalidRequest
		},
	}
	resp, err := fb.Generate(context.Background(), ModelRequest{}, "primary", reg, ProviderCapabilities{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Content != "custom" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "custom")
	}
}

func TestFallbackPolicy_ContextCancellation(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{err: &ModelError{Kind: ModelErrorUnavailable}}}}
	if err := reg.Register(ProviderEntry{ID: "fail", Model: model}); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fb := &FallbackPolicy{Chain: []ProviderID{"fail"}}
	_, err := fb.Generate(ctx, ModelRequest{}, "primary", reg, ProviderCapabilities{})
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestFallbackPolicy_Validate(t *testing.T) {
	tests := []struct {
		name    string
		fb      *FallbackPolicy
		wantErr bool
	}{
		{"nil", nil, true},
		{"empty chain", &FallbackPolicy{}, true},
		{"empty ID", &FallbackPolicy{Chain: []ProviderID{""}}, true},
		{"duplicate", &FallbackPolicy{Chain: []ProviderID{"a", "a"}}, true},
		{"valid", &FallbackPolicy{Chain: []ProviderID{"a", "b"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fb.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFallbackPolicy_IsRetryable(t *testing.T) {
	fb := &FallbackPolicy{}
	retryable := &ModelError{Kind: ModelErrorRateLimited}
	nonRetryable := &ModelError{Kind: ModelErrorAuthentication}

	if !fb.IsRetryable(retryable) {
		t.Error("expected rate_limited to be retryable")
	}
	if fb.IsRetryable(nonRetryable) {
		t.Error("expected authentication to not be retryable")
	}
	if fb.IsRetryable(nil) {
		t.Error("expected nil to not be retryable")
	}
}

func TestDefaultModelRetryable(t *testing.T) {
	if !DefaultModelRetryable(&ModelError{Kind: ModelErrorTimeout}) {
		t.Error("timeout should be retryable")
	}
	if DefaultModelRetryable(&ModelError{Kind: ModelErrorNotFound}) {
		t.Error("not_found should not be retryable")
	}
	if DefaultModelRetryable(nil) {
		t.Error("nil should not be retryable")
	}
}
