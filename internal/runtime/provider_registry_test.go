package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestProviderRegistry_RegisterAndGet(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	entry := ProviderEntry{ID: "test-provider", Model: model, Capabilities: ProviderCapabilities{SupportsTools: true}}
	if err := reg.Register(entry); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := reg.Get("test-provider")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "test-provider" {
		t.Errorf("ID = %q, want %q", got.ID, "test-provider")
	}
	if !got.Capabilities.SupportsTools {
		t.Error("Capabilities.SupportsTools = false, want true")
	}
}

func TestProviderRegistry_RegisterDuplicate(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}
	entry := ProviderEntry{ID: "dup", Model: model}

	if err := reg.Register(entry); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := reg.Register(entry)
	if err == nil {
		t.Fatal("expected error on duplicate register")
	}
	if !errors.Is(err, ErrProviderAlreadyExists) {
		t.Errorf("error = %v, want ErrProviderAlreadyExists", err)
	}
}

func TestProviderRegistry_GetNotFound(t *testing.T) {
	reg := NewProviderRegistry()
	_, err := reg.Get("missing")
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("error = %v, want ErrProviderNotFound", err)
	}
}

func TestProviderRegistry_List(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	for _, id := range []ProviderID{"a", "b", "c"} {
		if err := reg.Register(ProviderEntry{ID: id, Model: model}); err != nil {
			t.Fatalf("Register %q: %v", id, err)
		}
	}

	list := reg.List()
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	for i, want := range []ProviderID{"a", "b", "c"} {
		if list[i].ID != want {
			t.Errorf("List[%d].ID = %q, want %q", i, list[i].ID, want)
		}
	}
}

func TestProviderRegistry_IDs(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	for _, id := range []ProviderID{"x", "y"} {
		if err := reg.Register(ProviderEntry{ID: id, Model: model}); err != nil {
			t.Fatalf("Register %q: %v", id, err)
		}
	}

	ids := reg.IDs()
	if len(ids) != 2 {
		t.Fatalf("IDs len = %d, want 2", len(ids))
	}
	if ids[0] != "x" || ids[1] != "y" {
		t.Errorf("IDs = %v, want [x y]", ids)
	}
}

func TestProviderRegistry_Len(t *testing.T) {
	reg := NewProviderRegistry()
	if reg.Len() != 0 {
		t.Errorf("empty Len = %d, want 0", reg.Len())
	}
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}
	if err := reg.Register(ProviderEntry{ID: "one", Model: model}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("Len = %d, want 1", reg.Len())
	}
}

func TestProviderRegistry_RegisterValidation(t *testing.T) {
	reg := NewProviderRegistry()
	model := &stubModel{responses: []stubResponse{{resp: ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}}}}

	if err := reg.Register(ProviderEntry{Model: model}); err == nil {
		t.Error("expected error for missing ID")
	}
	if err := reg.Register(ProviderEntry{ID: "no-model"}); err == nil {
		t.Error("expected error for missing model")
	}
}

func TestProviderCapabilities_Satisfies(t *testing.T) {
	tests := []struct {
		name string
		caps ProviderCapabilities
		req  ProviderCapabilities
		want bool
	}{
		{"empty satisfies empty", ProviderCapabilities{}, ProviderCapabilities{}, true},
		{"tools not satisfied", ProviderCapabilities{}, ProviderCapabilities{SupportsTools: true}, false},
		{"tools satisfied", ProviderCapabilities{SupportsTools: true}, ProviderCapabilities{SupportsTools: true}, true},
		{"schema not satisfied", ProviderCapabilities{}, ProviderCapabilities{SupportsStructuredOutput: true}, false},
		{"schema satisfied", ProviderCapabilities{SupportsStructuredOutput: true}, ProviderCapabilities{SupportsStructuredOutput: true}, true},
		{"streaming not satisfied", ProviderCapabilities{}, ProviderCapabilities{SupportsStreaming: true}, false},
		{"streaming satisfied", ProviderCapabilities{SupportsStreaming: true}, ProviderCapabilities{SupportsStreaming: true}, true},
		{"all satisfied", ProviderCapabilities{SupportsTools: true, SupportsStructuredOutput: true, SupportsStreaming: true}, ProviderCapabilities{SupportsTools: true, SupportsStructuredOutput: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.caps.Satisfies(tt.req)
			if got != tt.want {
				t.Errorf("Satisfies = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProviderRegistry_NilReceiver(t *testing.T) {
	var reg *ProviderRegistry
	if reg.Len() != 0 {
		t.Errorf("nil Len = %d, want 0", reg.Len())
	}
	if reg.List() != nil {
		t.Error("nil List should return nil")
	}
	if reg.IDs() != nil {
		t.Error("nil IDs should return nil")
	}
	_, err := reg.Get("any")
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("nil Get error = %v, want ErrProviderNotFound", err)
	}
}

type stubModel struct {
	responses []stubResponse
	calls     int
}

type stubResponse struct {
	resp ModelResponse
	err  error
}

func (m *stubModel) Generate(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	if m.calls >= len(m.responses) {
		return ModelResponse{Message: Message{Role: RoleAssistant}, FinishReason: FinishReasonStop}, nil
	}
	r := m.responses[m.calls]
	m.calls++
	return r.resp, r.err
}
