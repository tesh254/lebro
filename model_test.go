package lebro

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelRequestValidate(t *testing.T) {
	t.Parallel()
	valid := ModelRequest{
		Model:    "example/model",
		Messages: []Message{{Role: RoleSystem, Content: "help"}, {Role: RoleUser, Content: "hello"}},
		Tools: []ToolDefinition{{
			ID: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"string"}`),
		}},
		OutputSchema: &ModelOutputSchema{Name: "answer", Description: "The answer.", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
		Extension:    json.RawMessage(`{"seed":7}`),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request error = %v", err)
	}

	tests := []struct {
		name string
		edit func(*ModelRequest)
		want string
	}{
		{name: "message", edit: func(request *ModelRequest) { request.Messages[0].Role = "invalid" }, want: "message 0"},
		{name: "tool ID", edit: func(request *ModelRequest) { request.Tools[0].ID = "" }, want: "requires an ID"},
		{name: "duplicate tool", edit: func(request *ModelRequest) { request.Tools = append(request.Tools, request.Tools[0]) }, want: "duplicate tool ID"},
		{name: "input schema", edit: func(request *ModelRequest) { request.Tools[0].InputSchema = json.RawMessage(`{`) }, want: "input schema"},
		{name: "output schema", edit: func(request *ModelRequest) { request.Tools[0].OutputSchema = json.RawMessage(`{`) }, want: "output schema"},
		{name: "empty requested output", edit: func(request *ModelRequest) { request.OutputSchema.Schema = nil }, want: "must not be empty"},
		{name: "invalid requested output", edit: func(request *ModelRequest) { request.OutputSchema.Schema = json.RawMessage(`{`) }, want: "must be valid JSON"},
		{name: "extension", edit: func(request *ModelRequest) { request.Extension = json.RawMessage(`{`) }, want: "request extension"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := cloneModelRequestForTest(valid)
			test.edit(&request)
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}

	if err := (ModelRequest{}).Validate(); err != nil {
		t.Fatalf("empty request error = %v", err)
	}
}

func TestModelResponseValidate(t *testing.T) {
	t.Parallel()
	valid := ModelResponse{
		Message: Message{Role: RoleAssistant, Content: "working", ToolCalls: modelToolCallsForTest(ModelToolCall{
			ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"id":"42"}`),
		}), StructuredOutput: NewModelStructuredOutput(json.RawMessage(`{"ok":true}`))},
		Usage:        ModelUsage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5},
		FinishReason: FinishReasonToolCalls,
		Extension:    json.RawMessage(`{"request_id":"req-1"}`),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid response error = %v", err)
	}

	tests := []struct {
		name string
		edit func(*ModelResponse)
		want string
	}{
		{name: "message", edit: func(response *ModelResponse) { response.Message.Role = "invalid" }, want: "model response"},
		{name: "message role", edit: func(response *ModelResponse) {
			response.Message.Role = RoleUser
			response.Message.ToolCalls = ModelToolCalls{}
			response.Message.StructuredOutput = ""
		}, want: "message role"},
		{name: "finish reason", edit: func(response *ModelResponse) { response.FinishReason = "vendor_reason" }, want: "finish reason"},
		{name: "usage", edit: func(response *ModelResponse) { response.Usage.InputTokens = -1 }, want: "negative"},
		{name: "calls without finish reason", edit: func(response *ModelResponse) { response.FinishReason = FinishReasonStop }, want: "requires tool_calls"},
		{name: "finish reason without calls", edit: func(response *ModelResponse) { response.Message.ToolCalls = ModelToolCalls{} }, want: "requires at least one"},
		{name: "structured output", edit: func(response *ModelResponse) {
			response.Message.StructuredOutput = NewModelStructuredOutput(json.RawMessage(`{`))
		}, want: "structured output"},
		{name: "extension", edit: func(response *ModelResponse) { response.Extension = json.RawMessage(`{`) }, want: "response extension"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := cloneModelResponseForTest(valid)
			test.edit(&response)
			if err := response.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}

	text := ModelResponse{Message: lebroAssistant("done"), FinishReason: FinishReasonStop}
	if err := text.Validate(); err != nil {
		t.Fatalf("text response error = %v", err)
	}
}

func TestModelToolCallValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call ModelToolCall
		want string
	}{
		{name: "valid", call: ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`null`)}},
		{name: "ID", call: ModelToolCall{ToolID: "lookup", Arguments: json.RawMessage(`null`)}, want: "requires an ID"},
		{name: "tool ID", call: ModelToolCall{ID: "call-1", Arguments: json.RawMessage(`null`)}, want: "requires a tool ID"},
		{name: "empty arguments", call: ModelToolCall{ID: "call-1", ToolID: "lookup"}, want: "valid JSON"},
		{name: "invalid arguments", call: ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{`)}, want: "valid JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call.Validate()
			if test.want == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestModelToolCallCollectionsAreDefensive(t *testing.T) {
	t.Parallel()
	arguments := json.RawMessage(`{"id":"42"}`)
	calls, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	arguments[0] = '['
	values := calls.Values()
	if got := string(values[0].Arguments); got != `{"id":"42"}` {
		t.Fatalf("constructor retained caller arguments: %s", got)
	}
	values[0].Arguments[0] = '['
	again := calls.Values()
	if got := string(again[0].Arguments); got != `{"id":"42"}` {
		t.Fatalf("Values retained caller arguments: %s", got)
	}
	var none ModelToolCalls
	if values := none.Values(); values != nil {
		t.Fatal("nil tool calls returned non-nil values")
	}
	structured := json.RawMessage(`{"ok":true}`)
	output := NewModelStructuredOutput(structured)
	structured[0] = '['
	if got := string(output.Raw()); got != `{"ok":true}` {
		t.Fatalf("structured output retained caller bytes: %s", got)
	}

	empty, err := NewModelToolCalls()
	if err != nil || !empty.IsZero() {
		t.Fatalf("empty tool calls = %#v, %v", empty, err)
	}
	if _, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid tool call encoding error = nil")
	}
	if _, err := NewModelToolCalls(
		ModelToolCall{ID: "same", ToolID: "lookup", Arguments: json.RawMessage(`{}`)},
		ModelToolCall{ID: "same", ToolID: "lookup", Arguments: json.RawMessage(`{}`)},
	); err == nil {
		t.Fatal("duplicate tool call encoding error = nil")
	}
	multiple, err := NewModelToolCalls(
		ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)},
		ModelToolCall{ID: "call-\x01", ToolID: "lookup", Arguments: json.RawMessage(`null`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if multipleValues := multiple.Values(); len(multipleValues) != 2 || multipleValues[1].ID != "call-\x01" {
		t.Fatalf("multiple tool calls = %#v", multipleValues)
	}
	if encoded, err := json.Marshal(ModelToolCalls{}); err != nil || string(encoded) != `[]` {
		t.Fatalf("empty tool call JSON = %s, %v", encoded, err)
	}
	if encoded, err := json.Marshal(multiple); err != nil || !json.Valid(encoded) || strings.Contains(string(encoded), `\x01`) {
		t.Fatalf("tool call JSON = %s, %v", encoded, err)
	}
	var decoded ModelToolCalls
	if err := json.Unmarshal([]byte("null"), &decoded); err != nil || !decoded.IsZero() {
		t.Fatalf("null tool calls = %#v, %v", decoded, err)
	}
	if err := decoded.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("invalid tool call unmarshal error = nil")
	}
	if err := decoded.UnmarshalJSON([]byte(`[{"id":"","tool_id":"lookup","arguments":{}}]`)); err == nil {
		t.Fatal("invalid decoded tool call error = nil")
	}
	canonical, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.UnmarshalJSON([]byte(`[ { "arguments": {}, "tool_id": "lookup", "id": "call-1" } ]`)); err != nil || decoded != canonical {
		t.Fatalf("canonical decoded tool calls = %#v, %v; want %#v", decoded, err, canonical)
	}
	equivalent, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"b":1,"a":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := NewModelToolCalls(ModelToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{ "a": 2, "b": 1 }`)})
	if err != nil {
		t.Fatal(err)
	}
	if equivalent != reordered {
		t.Fatalf("canonical tool call encoding = %s; want equal to %s", reordered, equivalent)
	}
	var nilCalls *ModelToolCalls
	if err := nilCalls.UnmarshalJSON([]byte("[]")); err == nil {
		t.Fatal("nil tool call receiver error = nil")
	}

	if encoded, err := json.Marshal(ModelStructuredOutput("")); err != nil || string(encoded) != "null" {
		t.Fatalf("empty structured output JSON = %s, %v", encoded, err)
	}
	if _, err := json.Marshal(ModelStructuredOutput("{")); err == nil {
		t.Fatal("invalid structured output JSON error = nil")
	}
	var decodedOutput ModelStructuredOutput
	if err := decodedOutput.UnmarshalJSON([]byte(`{"ok":true}`)); err != nil || string(decodedOutput.Raw()) != `{"ok":true}` {
		t.Fatalf("structured output unmarshal = %s, %v", decodedOutput, err)
	}
	if err := decodedOutput.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("invalid structured output unmarshal error = nil")
	}
	var nullOutput ModelStructuredOutput
	if err := nullOutput.UnmarshalJSON([]byte("null")); err != nil || nullOutput != "" {
		t.Fatalf("null structured output = %#v, %v; want empty", nullOutput, err)
	}
	var nilOutput *ModelStructuredOutput
	if err := nilOutput.UnmarshalJSON([]byte("null")); err == nil {
		t.Fatal("nil structured output receiver error = nil")
	}
}

func TestCanonicalFinishReasonsAndJSONHelper(t *testing.T) {
	t.Parallel()
	reasons := []FinishReason{
		FinishReasonStop, FinishReasonLength, FinishReasonToolCalls, FinishReasonContent,
		FinishReasonCancelled, FinishReasonUnspecified,
	}
	for _, reason := range reasons {
		if !validFinishReason(reason) {
			t.Fatalf("validFinishReason(%q) = false", reason)
		}
	}
	if validFinishReason("other") {
		t.Fatal("validFinishReason(other) = true")
	}
	if err := validateOptionalJSON("value", nil); err != nil {
		t.Fatalf("empty optional JSON error = %v", err)
	}
	if err := validateOptionalJSON("value", json.RawMessage(`true`)); err != nil {
		t.Fatalf("valid optional JSON error = %v", err)
	}
	if err := validateOptionalJSON("value", json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid optional JSON error = nil")
	}
}

func lebroAssistant(content string) Message {
	return Message{Role: RoleAssistant, Content: content}
}

func cloneModelRequestForTest(request ModelRequest) ModelRequest {
	request.Messages = append([]Message(nil), request.Messages...)
	request.Tools = append([]ToolDefinition(nil), request.Tools...)
	request.Tools[0].InputSchema = append(json.RawMessage(nil), request.Tools[0].InputSchema...)
	request.Tools[0].OutputSchema = append(json.RawMessage(nil), request.Tools[0].OutputSchema...)
	output := *request.OutputSchema
	output.Schema = append(json.RawMessage(nil), output.Schema...)
	request.OutputSchema = &output
	request.Extension = append(json.RawMessage(nil), request.Extension...)
	return request
}

func cloneModelResponseForTest(response ModelResponse) ModelResponse {
	response.Extension = append(json.RawMessage(nil), response.Extension...)
	return response
}
