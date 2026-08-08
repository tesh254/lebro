// The model-openai example runs a lebro OpenAI-compatible text-generation adapter
// against a recorded HTTP endpoint so it executes without a network call or an
// API key.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/openai"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	server := newFakeEndpoint()
	defer server.Close()

	model := mustValue(openai.New(openai.Config{
		APIKey:  "example-key",
		BaseURL: server.URL,
		Model:   "gpt-4o",
		Timeout: 0,
	}))

	ctx := context.Background()
	response, err := model.Generate(ctx, lebro.ModelRequest{
		Model:     "gpt-4o",
		Messages:  []lebro.Message{{Role: lebro.RoleUser, Content: "Say hello in one word."}},
		Extension: json.RawMessage(`{"temperature":0.2,"max_tokens":16}`),
	})
	if err != nil {
		return err
	}
	if err := response.Validate(); err != nil {
		return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Message: err.Error(), Err: err}
	}
	writef(output, "assistant: %s\n", response.Message.Content)
	writef(output, "usage: in=%d out=%d total=%d\n",
		response.Usage.InputTokens, response.Usage.OutputTokens, response.Usage.TotalTokens)
	writef(output, "finish: %s\n", response.FinishReason)

	if _, err := model.Generate(ctx, lebro.ModelRequest{
		Model:    "gpt-4o",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "fail"}},
	}); err != nil {
		var apiErr *lebro.ModelError
		if errors.As(err, &apiErr) {
			writef(output, "failure: kind=%s status=%d message=%s\n", apiErr.Kind, apiErr.StatusCode, apiErr.Message)
		} else {
			return err
		}
	}
	return nil
}

func newFakeEndpoint() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		if request["model"] != "gpt-4o" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		messages, _ := request["messages"].([]any)
		last, _ := messages[len(messages)-1].(map[string]any)
		content, _ := last["content"].(string)

		w.Header().Set("Content-Type", "application/json")
		if content == "fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream offline","type":"unavailable"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-example","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
