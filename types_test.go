package lebro

import (
	"encoding/json"
	"testing"
)

func TestMessageValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message Message
		wantErr bool
	}{
		{name: "user", message: Message{Role: RoleUser, Content: "hello"}},
		{name: "tool with call ID", message: Message{Role: RoleTool, ToolCallID: "call_1"}},
		{name: "assistant tool calls", message: Message{Role: RoleAssistant, ToolCalls: modelToolCallsForTest(ModelToolCall{ID: "call_1", ToolID: "echo", Arguments: json.RawMessage(`{}`)})}},
		{name: "unknown role", message: Message{Role: "unknown"}, wantErr: true},
		{name: "tool without call ID", message: Message{Role: RoleTool}, wantErr: true},
		{name: "assistant with tool result ID", message: Message{Role: RoleAssistant, ToolCallID: "call_1"}, wantErr: true},
		{name: "tool calls on user", message: Message{Role: RoleUser, ToolCalls: modelToolCallsForTest(ModelToolCall{ID: "call_1", ToolID: "echo", Arguments: json.RawMessage(`{}`)})}, wantErr: true},
		{name: "structured output on user", message: Message{Role: RoleUser, StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{}`))}, wantErr: true},
		{name: "invalid structured output", message: Message{Role: RoleAssistant, StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{`))}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.message.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAssistantToolCallsRoundTripInTranscriptJSON(t *testing.T) {
	t.Parallel()
	want := Message{Role: RoleAssistant, Content: "checking", ToolCalls: modelToolCallsForTest(ModelToolCall{
		ID: "call_1", ToolID: "weather", Arguments: json.RawMessage(`{"city":"Nairobi"}`),
	}), StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{"temperature_c":24.5}`))}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestMessageRemainsComparable(t *testing.T) {
	t.Parallel()
	message := Message{Role: RoleAssistant, ToolCalls: modelToolCallsForTest(ModelToolCall{
		ID: "call_1", ToolID: "echo", Arguments: json.RawMessage(`{}`),
	})}
	messages := map[Message]string{message: "stored"}
	if messages[message] != "stored" {
		t.Fatal("comparable message could not be used as a map key")
	}
}

func modelToolCallsForTest(calls ...ModelToolCall) ModelToolCalls {
	encoded, err := NewModelToolCalls(calls...)
	if err != nil {
		panic(err)
	}
	return encoded
}
