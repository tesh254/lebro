package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
)

// newTestClient builds a client pointed at a handler served by httptest.
func newTestClient(t *testing.T, handler http.HandlerFunc) *httpapi.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := httpapi.NewClient(httpapi.ClientConfig{BaseURL: server.URL})
	must(t, err)
	return client
}

// writeSSE writes raw Server-Sent Event bytes, flushing so the client reads
// them incrementally rather than as one buffered body.
func writeSSE(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("response writer is not a flusher")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for _, frame := range frames {
		if _, err := w.Write([]byte(frame)); err != nil {
			return
		}
		flusher.Flush()
	}
}

func TestNewClientRejectsBadConfig(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":       "",
		"no scheme":   "example.com/api",
		"bad scheme":  "ftp://example.com",
		"no host":     "http://",
		"unparseable": "http://exa mple.com/\x7f",
	}
	for name, baseURL := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := httpapi.NewClient(httpapi.ClientConfig{BaseURL: baseURL}); err == nil {
				t.Fatalf("NewClient(%q) succeeded, want error", baseURL)
			}
		})
	}
}

// TestClientErrorCodeMapsToSentinel is the core of the ticket's "map API errors
// to the library's typed errors" requirement: every code the server can send
// must reach the caller as an *APIError that carries the code and unwraps to
// the lebro sentinel standing for it.
func TestClientErrorCodeMapsToSentinel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code     httpapi.ErrorCode
		status   int
		sentinel error
	}{
		{httpapi.ErrorCodeInvalidRequest, http.StatusBadRequest, nil},
		// invalid_input and invalid_output are produced by both agent and
		// workflow failures, so no single sentinel is right for either.
		{httpapi.ErrorCodeInvalidInput, http.StatusBadRequest, nil},
		{httpapi.ErrorCodeNotFound, http.StatusNotFound, lebro.ErrNotFound},
		{httpapi.ErrorCodeInvalidOutput, http.StatusBadGateway, nil},
		{httpapi.ErrorCodeToolFailure, http.StatusInternalServerError, lebro.ErrAgentToolFailure},
		{httpapi.ErrorCodeStepFailure, http.StatusInternalServerError, lebro.ErrWorkflowStepFailure},
		{httpapi.ErrorCodeProviderFailure, http.StatusBadGateway, lebro.ErrAgentProviderFailure},
		{httpapi.ErrorCodeTimeout, http.StatusGatewayTimeout, lebro.ErrAgentTimeout},
		{httpapi.ErrorCodeStepLimitExhausted, http.StatusBadGateway, lebro.ErrAgentStepLimitExhausted},
		{httpapi.ErrorCodeCancelled, 499, lebro.ErrAgentCancelled},
		{httpapi.ErrorCodeMethodNotAllowed, http.StatusMethodNotAllowed, nil},
		{httpapi.ErrorCodeInternal, http.StatusInternalServerError, nil},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.code), func(t *testing.T) {
			t.Parallel()

			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": testCase.code, "message": "boom"},
				})
			})

			_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
			if err == nil {
				t.Fatal("Run succeeded, want error")
			}

			var apiErr *httpapi.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v is not an *APIError", err)
			}
			if apiErr.Code != testCase.code {
				t.Errorf("code = %q, want %q", apiErr.Code, testCase.code)
			}
			if apiErr.StatusCode != testCase.status {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, testCase.status)
			}
			if testCase.sentinel != nil && !errors.Is(err, testCase.sentinel) {
				t.Errorf("error %v does not match sentinel %v", err, testCase.sentinel)
			}
			// A code with no unambiguous sentinel must not claim one: a wrong
			// match is worse than no match, because it silently mis-branches.
			if testCase.sentinel == nil {
				for _, wrong := range []error{
					lebro.ErrWorkflowInvalidStepInput,
					lebro.ErrWorkflowInvalidStepOutput,
					lebro.ErrAgentToolFailure,
					lebro.ErrNotFound,
				} {
					if errors.Is(err, wrong) {
						t.Errorf("code %q falsely matches %v", testCase.code, wrong)
					}
				}
			}
		})
	}
}

// TestClientCancelledErrorMatchesContextCanceled records that a caller may
// check either the lebro sentinel or context.Canceled for a cancelled run.
func TestClientCancelledErrorMatchesContextCanceled(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(499)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": httpapi.ErrorCodeCancelled, "message": "cancelled"},
		})
	})

	_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not match context.Canceled", err)
	}
	if !errors.Is(err, lebro.ErrAgentCancelled) {
		t.Errorf("error %v does not match lebro.ErrAgentCancelled", err)
	}
}

// TestClientUndecodableErrorBodyFallsBackToStatus covers a proxy that replaces
// the server's JSON error with its own HTML page: the caller still gets a
// classification rather than a bare decode failure.
func TestClientUndecodableErrorBodyFallsBackToStatus(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>gateway says no</html>"))
	})

	_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != httpapi.ErrorCodeNotFound {
		t.Errorf("code = %q, want %q", apiErr.Code, httpapi.ErrorCodeNotFound)
	}
	if !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("error %v does not match lebro.ErrNotFound", err)
	}
}

// TestClientUnknownErrorCodeReportsInternal covers a server newer than the
// client that sends a code this build does not know.
func TestClientUnknownErrorCodeReportsInternal(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "quantum_flux", "message": "from the future"},
		})
	})

	_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != httpapi.ErrorCodeInternal {
		t.Errorf("code = %q, want %q", apiErr.Code, httpapi.ErrorCodeInternal)
	}
	// The message paired with an unknown code is not part of this contract
	// either, so it must not reach the caller under a code claiming to be the
	// fixed public one.
	if strings.Contains(apiErr.Message, "from the future") {
		t.Errorf("message = %q, want the canonical text for internal_error", apiErr.Message)
	}
}

func TestClientMalformedSuccessBody(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not json"))
	})

	_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
	if !errors.Is(err, httpapi.ErrMalformedResponse) {
		t.Fatalf("error = %v, want ErrMalformedResponse", err)
	}
}

func TestClientEmptySuccessBody(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
	if !errors.Is(err, httpapi.ErrMalformedResponse) {
		t.Fatalf("error = %v, want ErrMalformedResponse", err)
	}
}

// TestClientSendsRequestShape asserts the client addresses the documented route
// and sends the documented body, since a client that decodes correctly but
// calls the wrong URL passes every response-shaped test.
func TestClientSendsRequestShape(t *testing.T) {
	t.Parallel()

	type captured struct {
		method string
		path   string
		query  string
		body   string
		auth   string
	}
	got := make(chan captured, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		got <- captured{
			method: r.Method,
			path:   r.URL.EscapedPath(),
			query:  r.URL.RawQuery,
			body:   strings.TrimSpace(string(body)),
			auth:   r.Header.Get("Authorization"),
		}
		_ = json.NewEncoder(w).Encode(httpapi.RunResponse{RunID: "run-1", Status: "succeeded"})
	}))
	t.Cleanup(server.Close)

	client, err := httpapi.NewClient(httpapi.ClientConfig{
		BaseURL: server.URL,
		Header: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer token")
		},
	})
	must(t, err)

	_, err = client.Run(context.Background(), "my agent",
		httpapi.RunRequest{Messages: []httpapi.MessageInput{{Content: "hi"}}},
		httpapi.WithThread("thread-1"),
	)
	must(t, err)

	result := <-got
	if result.method != http.MethodPost {
		t.Errorf("method = %q, want POST", result.method)
	}
	if result.path != "/agents/my%20agent/runs" {
		t.Errorf("path = %q, want /agents/my%%20agent/runs", result.path)
	}
	if result.query != "thread_id=thread-1" {
		t.Errorf("query = %q, want thread_id=thread-1", result.query)
	}
	if !strings.Contains(result.body, `"content":"hi"`) {
		t.Errorf("body = %q, want it to carry the message content", result.body)
	}
	if result.auth != "Bearer token" {
		t.Errorf("Authorization = %q, want the Header hook to have run", result.auth)
	}
}

// TestClientPreservesBaseURLPath covers a server mounted under a path prefix; a
// client that discarded the prefix would address the wrong routes.
func TestClientPreservesBaseURLPath(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_ = json.NewEncoder(w).Encode(httpapi.HealthResponse{Status: "ok"})
	}))
	t.Cleanup(server.Close)

	// The trailing slash must not produce a doubled separator.
	client, err := httpapi.NewClient(httpapi.ClientConfig{BaseURL: server.URL + "/lebro/"})
	must(t, err)

	_, err = client.Health(context.Background())
	must(t, err)

	if path := <-paths; path != "/lebro/health" {
		t.Errorf("path = %q, want /lebro/health", path)
	}
}

func TestClientRejectsEmptyIdentifiers(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server was called for an empty identifier")
		w.WriteHeader(http.StatusOK)
	})
	ctx := context.Background()

	if _, err := client.Run(ctx, "", httpapi.RunRequest{}); err == nil {
		t.Error("Run with empty agent ID succeeded, want error")
	}
	if _, err := client.RunStream(ctx, "", httpapi.RunRequest{}); err == nil {
		t.Error("RunStream with empty agent ID succeeded, want error")
	}
	if _, err := client.RunWorkflow(ctx, "", httpapi.WorkflowRunRequest{}); err == nil {
		t.Error("RunWorkflow with empty workflow ID succeeded, want error")
	}
	if _, err := client.GetThread(ctx, ""); err == nil {
		t.Error("GetThread with empty thread ID succeeded, want error")
	}
	if _, err := client.ListMessages(ctx, ""); err == nil {
		t.Error("ListMessages with empty thread ID succeeded, want error")
	}
}

// TestClientMessageOptionsAreOmittedWhenUnset asserts the zero-valued options
// send no query parameter, so the server applies its own defaults rather than
// receiving "limit=0".
func TestClientMessageOptionsAreOmittedWhenUnset(t *testing.T) {
	t.Parallel()

	queries := make(chan string, 1)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(httpapi.MessageListResponse{})
	})

	_, err := client.ListMessages(context.Background(), "thread-1", httpapi.WithCursor(""), httpapi.WithLimit(0))
	must(t, err)

	if query := <-queries; query != "" {
		t.Errorf("query = %q, want empty", query)
	}
}

func TestClientStreamDeliversDeltasThenTerminal(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			"event: model_delta\ndata: {\"type\":\"model_delta\",\"text\":\"Hel\"}\n\n",
			"event: model_delta\ndata: {\"type\":\"model_delta\",\"text\":\"lo\"}\n\n",
			"event: run_succeeded\ndata: {\"type\":\"run_succeeded\",\"run_id\":\"run-1\",\"status\":\"succeeded\",\"text\":\"Hello\"}\n\n",
		)
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	var text strings.Builder
	var count int
	for event := range stream.Events {
		count++
		if event.Type != "model_delta" {
			t.Errorf("event type = %q, want model_delta; terminal events must not reach Events", event.Type)
		}
		text.WriteString(event.Text)
	}
	if count != 2 {
		t.Errorf("delta count = %d, want 2", count)
	}
	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want %q", text.String(), "Hello")
	}

	result, err := stream.Wait()
	must(t, err)
	if result.RunID != "run-1" || result.Status != "succeeded" || result.Content != "Hello" {
		t.Errorf("result = %+v, want run-1/succeeded/Hello", result)
	}
}

// TestClientStreamTerminalFailureIsTyped asserts a run that fails mid-stream
// reaches the caller with the same classification the non-streaming route
// would have produced, and with no HTTP status, since the response was 200.
func TestClientStreamTerminalFailureIsTyped(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			"event: run_failed\ndata: {\"type\":\"run_failed\",\"run_id\":\"run-1\",\"status\":\"failed\",\"error\":{\"code\":\"tool_failure\",\"message\":\"a tool failed during the run\"}}\n\n",
		)
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	_, err = stream.Drain()
	if !errors.Is(err, lebro.ErrAgentToolFailure) {
		t.Fatalf("error = %v, want it to match lebro.ErrAgentToolFailure", err)
	}
	var apiErr *httpapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.StatusCode != 0 {
		t.Errorf("status = %d, want 0 for a terminal-event error", apiErr.StatusCode)
	}
}

// TestClientStreamWithoutTerminalEvent is the dropped-connection case: the
// stream contract guarantees a terminal event, so its absence must be an error
// rather than a silent empty success.
func TestClientStreamWithoutTerminalEvent(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, "event: model_delta\ndata: {\"type\":\"model_delta\",\"text\":\"partial\"}\n\n")
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	_, err = stream.Drain()
	if !errors.Is(err, httpapi.ErrStreamIncomplete) {
		t.Fatalf("error = %v, want ErrStreamIncomplete", err)
	}
}

// TestClientStreamMultiLineData covers a proxy that re-wraps the server's
// single data line into several: the SSE specification joins them with a
// newline, and a client that read only the first would truncate the payload.
func TestClientStreamMultiLineData(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			"event: run_succeeded\ndata: {\"type\":\"run_succeeded\",\ndata: \"run_id\":\"run-1\",\"status\":\"succeeded\",\"text\":\"ok\"}\n\n",
		)
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	result, err := stream.Drain()
	must(t, err)
	if result.RunID != "run-1" || result.Content != "ok" {
		t.Errorf("result = %+v, want run-1/ok", result)
	}
}

// TestClientStreamIgnoresCommentsAndUnknownFields asserts keep-alive comments
// and forward-compatible fields do not break the reader.
func TestClientStreamIgnoresCommentsAndUnknownFields(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			": keep-alive\n\n",
			"id: 7\nretry: 100\nevent: model_delta\ndata: {\"type\":\"model_delta\",\"text\":\"hi\"}\n\n",
			"event: run_succeeded\ndata: {\"type\":\"run_succeeded\",\"status\":\"succeeded\",\"text\":\"hi\"}\n\n",
		)
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	var count int
	for range stream.Events {
		count++
	}
	if count != 1 {
		t.Errorf("delta count = %d, want 1", count)
	}
	if _, err := stream.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestClientStreamMalformedFrame covers a frame whose data is not JSON.
func TestClientStreamMalformedFrame(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, "event: model_delta\ndata: {not json\n\n")
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	if _, err := stream.Drain(); !errors.Is(err, httpapi.ErrMalformedResponse) {
		t.Fatalf("error = %v, want ErrMalformedResponse", err)
	}
}

// TestClientStreamPreStreamErrorIsReturnedImmediately asserts a failure the
// server discovers before writing any bytes is returned from RunStream itself,
// not deferred to Wait, so a caller does not have to set up a drain loop to
// learn the agent does not exist.
func TestClientStreamPreStreamErrorIsReturnedImmediately(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": httpapi.ErrorCodeNotFound, "message": "nope"},
		})
	})

	stream, err := client.RunStream(context.Background(), "missing", httpapi.RunRequest{})
	if err == nil {
		stream.Cancel()
		t.Fatal("RunStream succeeded, want error")
	}
	if stream != nil {
		t.Error("RunStream returned a non-nil stream alongside an error")
	}
	if !errors.Is(err, lebro.ErrNotFound) {
		t.Errorf("error %v does not match lebro.ErrNotFound", err)
	}
}

// TestClientStreamCancelReleasesAbandonedStream is the goroutine-leak case: a
// caller that stops reading partway must not park the reader forever on a
// channel with no receiver.
//
// The server writes every frame in one response rather than pacing them, which
// is what makes this test load-bearing. When frames arrive one at a time,
// Cancel tears down the connection and the reader exits on the resulting read
// error before it ever attempts another send — so the test would pass even
// with the cancellation-aware send removed, and would be proving nothing. With
// the whole body already buffered client-side, the reader has deltas to
// forward that need no further network activity, so the only thing that can
// release it is the send itself observing cancellation.
//
// Verified by reverting the select in ClientStream.read to a bare channel
// send: this test then hangs until its timeout, and goleak in TestMain reports
// the parked goroutine.
func TestClientStreamCancelReleasesAbandonedStream(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	for range 50 {
		body.WriteString("event: model_delta\ndata: {\"type\":\"model_delta\",\"text\":\"x\"}\n\n")
	}
	body.WriteString("event: run_succeeded\ndata: {\"type\":\"run_succeeded\",\"status\":\"succeeded\"}\n\n")

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body.String()))
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)

	// Read exactly one delta, then abandon the stream without draining. The
	// remaining 49 are buffered and have nobody to receive them.
	<-stream.Events
	stream.Cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, waitErr := stream.Wait(); !errors.Is(waitErr, context.Canceled) {
			t.Errorf("Wait error = %v, want it to match context.Canceled", waitErr)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait blocked after Cancel on an abandoned stream")
	}
}

// TestClientStreamContextCancellationStopsRun asserts cancelling the caller's
// context ends the stream and reports a cancelled run.
func TestClientStreamContextCancellationStopsRun(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: model_delta\ndata: {\"type\":\"model_delta\",\"text\":\"x\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
		close(released)
	})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.RunStream(ctx, "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	<-stream.Events
	cancel()

	if _, err := stream.Drain(); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to match context.Canceled", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not observe the cancelled request")
	}
}

func TestClientNilStreamMethodsAreSafe(t *testing.T) {
	t.Parallel()

	var stream *httpapi.ClientStream
	stream.Cancel()
	if _, err := stream.Wait(); err == nil {
		t.Error("Wait on a nil stream succeeded, want error")
	}
	if _, err := stream.Drain(); err == nil {
		t.Error("Drain on a nil stream succeeded, want error")
	}
}

// TestClientNilReceiversAreSafe asserts the nil paths report an error rather
// than panicking, matching how the rest of the module treats a nil receiver.
func TestClientNilReceiversAreSafe(t *testing.T) {
	t.Parallel()

	var client *httpapi.Client
	ctx := context.Background()

	if _, err := client.Run(ctx, "assistant", httpapi.RunRequest{}); err == nil {
		t.Error("Run on a nil client succeeded, want error")
	}
	if _, err := client.RunStream(ctx, "assistant", httpapi.RunRequest{}); err == nil {
		t.Error("RunStream on a nil client succeeded, want error")
	}
	if _, err := client.Health(ctx); err == nil {
		t.Error("Health on a nil client succeeded, want error")
	}
	if err := client.CheckCompatibility(ctx); err == nil {
		t.Error("CheckCompatibility on a nil client succeeded, want error")
	}

	var apiErr *httpapi.APIError
	if apiErr.Error() != "" {
		t.Error("nil APIError.Error is non-empty")
	}
	if apiErr.Unwrap() != nil {
		t.Error("nil APIError.Unwrap is non-nil")
	}
}

// TestClientZeroValueConfigWorks asserts the documented defaults: a nil
// HTTPClient selects a usable one and a nil Header hook is skipped, so the
// minimal configuration is a working one.
func TestClientZeroValueConfigWorks(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("Accept = %q, want application/json", accept)
		}
		_ = json.NewEncoder(w).Encode(httpapi.HealthResponse{Status: "ok"})
	})

	health, err := client.Health(context.Background())
	must(t, err)
	if health.Status != "ok" {
		t.Errorf("status = %q, want ok", health.Status)
	}
}

// TestClientNilOptionsAreIgnored asserts a nil option entry is skipped rather
// than dereferenced, so a caller building options conditionally does not have
// to filter the slice.
func TestClientNilOptionsAreIgnored(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()

	if _, err := client.Run(ctx, "assistant", httpapi.RunRequest{}, nil); err != nil {
		t.Errorf("Run with a nil option: %v", err)
	}
	if _, err := client.ListMessages(ctx, "thread-1", nil); err != nil {
		t.Errorf("ListMessages with a nil option: %v", err)
	}
}

func TestClientCheckCompatibility(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		info     map[string]any
		wantErr  error
		wantPass bool
	}{
		{
			name:     "same version",
			info:     map[string]any{"x-lebro-contract-version": httpapi.ContractVersion},
			wantPass: true,
		},
		{
			name:     "newer minor is compatible",
			info:     map[string]any{"x-lebro-contract-version": "1.7.3"},
			wantPass: true,
		},
		{
			name:    "different major",
			info:    map[string]any{"x-lebro-contract-version": "2.0.0"},
			wantErr: httpapi.ErrIncompatibleContract,
		},
		{
			name:    "absent version",
			info:    map[string]any{},
			wantErr: httpapi.ErrIncompatibleContract,
		},
		{
			name:    "non-string version",
			info:    map[string]any{"x-lebro-contract-version": 3},
			wantErr: httpapi.ErrMalformedResponse,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "info": testCase.info})
			})

			err := client.CheckCompatibility(context.Background())
			if testCase.wantPass {
				must(t, err)
				return
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

// TestClientHandlesNullJSONBodies covers the decoding case that fails silently
// rather than loudly: JSON null decodes into a map or a struct field without an
// error, leaving a nil map that panics on write and a zero value that is
// indistinguishable from a real one. A server or proxy emitting null must
// produce a classified error, not a panic and not a false success.
func TestClientHandlesNullJSONBodies(t *testing.T) {
	t.Parallel()

	t.Run("null info object", func(t *testing.T) {
		t.Parallel()
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"openapi":"3.1.0","info":null}`))
		})
		if err := client.CheckCompatibility(context.Background()); !errors.Is(err, httpapi.ErrIncompatibleContract) {
			t.Fatalf("error = %v, want ErrIncompatibleContract", err)
		}
	})

	t.Run("null document", func(t *testing.T) {
		t.Parallel()
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`null`))
		})
		if err := client.CheckCompatibility(context.Background()); !errors.Is(err, httpapi.ErrIncompatibleContract) {
			t.Fatalf("error = %v, want ErrIncompatibleContract", err)
		}
	})

	t.Run("null error body", func(t *testing.T) {
		t.Parallel()
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":null}`))
		})
		_, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
		var apiErr *httpapi.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("error %v is not an *APIError", err)
		}
		if apiErr.Code != httpapi.ErrorCodeInternal {
			t.Errorf("code = %q, want internal_error", apiErr.Code)
		}
	})

	t.Run("null run response", func(t *testing.T) {
		t.Parallel()
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`null`))
		})
		// A null 200 body decodes without error into the zero value. It is not
		// a malformed response, but it must not be reported as a real run
		// either: the zero RunID and status are what a caller sees.
		result, err := client.Run(context.Background(), "assistant", httpapi.RunRequest{})
		must(t, err)
		if result.RunID != "" || result.Status != "" {
			t.Errorf("result = %+v, want the zero value", result)
		}
	})
}

// TestClientEscapesPathSegmentsExactlyOnce is a regression test: assigning an
// already-escaped path to url.URL.Path makes it re-escape on render, so "%2F"
// went on the wire as "%252F" and the server reported not-found for an agent it
// exposes. The assertion is on the escaped path, which is what the mux routes
// on — the decoded Path field looks correct either way, which is how the bug
// survived the first round of tests.
func TestClientEscapesPathSegmentsExactlyOnce(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id   string
		want string
	}{
		{"team/assistant", "/agents/team%2Fassistant/runs"},
		{"my agent", "/agents/my%20agent/runs"},
		{"café", "/agents/caf%C3%A9/runs"},
		{"plain", "/agents/plain/runs"},
	}

	for _, testCase := range cases {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()

			got := make(chan string, 1)
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				got <- r.URL.EscapedPath()
				_ = json.NewEncoder(w).Encode(httpapi.RunResponse{RunID: "run-1"})
			})

			_, err := client.Run(context.Background(), testCase.id, httpapi.RunRequest{})
			must(t, err)

			if path := <-got; path != testCase.want {
				t.Errorf("wire path = %q, want %q", path, testCase.want)
			}
		})
	}
}

// TestClientDeadlineIsDistinctFromCancellation asserts an elapsed deadline is
// not reported as an explicit cancellation. The in-process runtime preserves
// the distinction, and a caller that retries on DeadlineExceeded but not on
// Canceled would mis-branch if the client collapsed them.
func TestClientDeadlineIsDistinctFromCancellation(t *testing.T) {
	t.Parallel()

	// The handler must not block on the request context alone: httptest's
	// Close waits for outstanding handlers, so a handler parked until the
	// client's context ends would keep the server alive past the test.
	// Releasing on either the request context or an explicit channel keeps
	// cleanup prompt.
	// The handler stalls just long enough for the client's context to expire
	// first, which is the event under test. It returns on its own rather than
	// waiting for the request context, because httptest's Close waits for
	// outstanding handlers and a handler parked on the connection would keep
	// the server alive well past the assertion.
	newBlockingClient := func(t *testing.T) *httpapi.Client {
		t.Helper()
		return newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
		})
	}

	t.Run("deadline", func(t *testing.T) {
		client := newBlockingClient(t)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		_, err := client.Run(ctx, "assistant", httpapi.RunRequest{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not match context.DeadlineExceeded", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("error %v falsely matches context.Canceled", err)
		}
		if !errors.Is(err, lebro.ErrAgentCancelled) {
			t.Errorf("error %v does not match lebro.ErrAgentCancelled", err)
		}
	})

	t.Run("explicit cancel", func(t *testing.T) {
		client := newBlockingClient(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		_, err := client.Run(ctx, "assistant", httpapi.RunRequest{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error %v does not match context.Canceled", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v falsely matches context.DeadlineExceeded", err)
		}
	})
}

// TestClientStreamBoundsAccumulatedFrame is a regression test: the scanner's
// limit bounds one physical line, so a frame split across many under-limit data
// lines could accumulate without bound — 40 lines of 0.5MB each allocate ~20MB
// against a 1MB frame limit.
//
// The payload is valid JSON when joined, so the failure must come from the size
// check rather than from a decode error. An oversized frame of garbage would
// fail either way and would not distinguish the bound from its absence.
func TestClientStreamBoundsAccumulatedFrame(t *testing.T) {
	t.Parallel()

	const (
		lineCount = 40
		lineBytes = 500 * 1024
	)
	// Split one long JSON string value across many data lines. Joined with
	// newlines the result is still a well-formed StreamEvent, so a client that
	// accepted it would decode successfully — which is exactly the outcome the
	// bound has to prevent.
	chunk := strings.Repeat("A", lineBytes)

	var body strings.Builder
	body.WriteString("event: model_delta\n")
	body.WriteString(`data: {"type":"model_delta","text":"`)
	body.WriteString("\n")
	for range lineCount {
		body.WriteString("data: " + chunk + "\n")
	}
	body.WriteString(`data: "}`)
	body.WriteString("\n\n")

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body.String()))
	})

	stream, err := client.RunStream(context.Background(), "assistant", httpapi.RunRequest{})
	must(t, err)
	defer stream.Cancel()

	_, err = stream.Drain()
	if !errors.Is(err, httpapi.ErrMalformedResponse) {
		t.Fatalf("error = %v, want ErrMalformedResponse for an oversized frame", err)
	}
	// Distinguish the size check from a decode failure: without the bound the
	// frame decodes cleanly and Drain returns no error at all.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to report the size bound rather than a decode failure", err)
	}
}

func TestClientCheckCompatibilityRejectsMalformedDocument(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a document"))
	})

	if err := client.CheckCompatibility(context.Background()); !errors.Is(err, httpapi.ErrMalformedResponse) {
		t.Fatalf("error = %v, want ErrMalformedResponse", err)
	}
}
