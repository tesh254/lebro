package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

func TestNewEmbedderValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  EmbedderConfig
		wantErr string
	}{
		{
			name:   "valid",
			config: EmbedderConfig{APIKey: "k", Model: "text-embedding-3-small", Dimension: 1536},
		},
		{
			name:    "missing API key",
			config:  EmbedderConfig{Model: "m", Dimension: 8},
			wantErr: "API key is required",
		},
		{
			name:    "missing model",
			config:  EmbedderConfig{APIKey: "k", Dimension: 8},
			wantErr: "embedding model is required",
		},
		{
			name:    "zero dimension",
			config:  EmbedderConfig{APIKey: "k", Model: "m"},
			wantErr: "dimension must be positive",
		},
		{
			name:    "negative dimension",
			config:  EmbedderConfig{APIKey: "k", Model: "m", Dimension: -1},
			wantErr: "dimension must be positive",
		},
		{
			name:    "relative base URL",
			config:  EmbedderConfig{APIKey: "k", Model: "m", Dimension: 8, BaseURL: "/v1"},
			wantErr: "must be absolute",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			embedder, err := NewEmbedder(test.config)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("NewEmbedder error = %v, want nil", err)
				}
				if embedder.Dimension() != test.config.Dimension {
					t.Fatalf("Dimension() = %d, want %d", embedder.Dimension(), test.config.Dimension)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewEmbedder error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestEmbedderEmbed(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotUserAgent string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1"+embeddingsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, "/v1"+embeddingsPath)
		}
		gotAuth = r.Header.Get("Authorization")
		gotUserAgent = r.Header.Get("User-Agent")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"text-embedding-3-small",
			"data":[
				{"index":0,"embedding":[1,0,0,0]},
				{"index":1,"embedding":[0,1,0,0]}
			],
			"usage":{"prompt_tokens":4,"total_tokens":4}
		}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{
		BaseURL:   server.URL + "/v1",
		APIKey:    "test-key",
		Model:     "text-embedding-3-small",
		Dimension: 4,
	})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	vectors, err := embedder.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed error = %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("len(vectors) = %d, want 2", len(vectors))
	}
	if vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Fatalf("vectors = %v, want the response order preserved", vectors)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotUserAgent != defaultEmbedderUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, defaultEmbedderUserAgent)
	}
	if gotBody["model"] != "text-embedding-3-small" {
		t.Fatalf("request model = %v, want %q", gotBody["model"], "text-embedding-3-small")
	}
	// dimensions must be absent unless explicitly requested, since some
	// gateways reject the parameter.
	if _, exists := gotBody["dimensions"]; exists {
		t.Fatal("request carries dimensions without RequestDimension set")
	}
}

func TestEmbedderRequestDimension(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{
		BaseURL:          server.URL,
		APIKey:           "k",
		Model:            "m",
		Dimension:        2,
		RequestDimension: true,
	})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed error = %v", err)
	}

	dimensions, ok := gotBody["dimensions"].(float64)
	if !ok || int(dimensions) != 2 {
		t.Fatalf("request dimensions = %v, want 2", gotBody["dimensions"])
	}
}

// TestEmbedderReordersResponse is why the adapter sorts by index rather than
// trusting wire order: the API guarantees an index per item, not an order.
func TestEmbedderReordersResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"index":2,"embedding":[0,0,1]},
			{"index":0,"embedding":[1,0,0]},
			{"index":1,"embedding":[0,1,0]}
		]}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 3})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	vectors, err := embedder.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed error = %v", err)
	}
	// Input order must be restored, so vector i belongs to input i.
	want := [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	for i, expected := range want {
		for j, value := range expected {
			if vectors[i][j] != value {
				t.Fatalf("vectors[%d] = %v, want %v", i, vectors[i], expected)
			}
		}
	}
}

func TestEmbedderRejectsWrongCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Two inputs requested, one embedding returned.
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"a", "b"})
	assertModelErrorKind(t, err, lebro.ModelErrorMalformedResponse, nil)
	if !strings.Contains(err.Error(), "embeddings for") {
		t.Fatalf("error = %q, want it to report the count mismatch", err.Error())
	}
}

func TestEmbedderRejectsWrongDimension(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0,0,0]}]}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"a"})
	assertModelErrorKind(t, err, lebro.ModelErrorMalformedResponse, nil)
	if !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("error = %q, want it to report the dimension mismatch", err.Error())
	}
}

// TestEmbedderRejectsDuplicateIndices covers a provider that returns the right
// count with duplicate indices, which would otherwise misalign vectors with
// inputs after sorting.
func TestEmbedderRejectsDuplicateIndices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"index":0,"embedding":[1,0]},
			{"index":0,"embedding":[0,1]}
		]}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"a", "b"})
	assertModelErrorKind(t, err, lebro.ModelErrorMalformedResponse, nil)
	if !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("error = %q, want it to report the index defect", err.Error())
	}
}

func TestEmbedderRejectsEmptyInput(t *testing.T) {
	embedder, err := NewEmbedder(EmbedderConfig{APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), nil)
	assertModelErrorKind(t, err, lebro.ModelErrorInvalidRequest, nil)

	_, err = embedder.Embed(context.Background(), []string{"ok", ""})
	assertModelErrorKind(t, err, lebro.ModelErrorInvalidRequest, nil)
}

func TestEmbedderClassifiesHTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		wantKind   lebro.ModelErrorKind
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantKind: lebro.ModelErrorAuthentication},
		{name: "forbidden", status: http.StatusForbidden, wantKind: lebro.ModelErrorPermissionDenied},
		{name: "not found", status: http.StatusNotFound, wantKind: lebro.ModelErrorNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, retryAfter: "7", wantKind: lebro.ModelErrorRateLimited},
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":{"message":"bad input","type":"invalid_request_error"}}`, wantKind: lebro.ModelErrorInvalidRequest},
		{name: "server error", status: http.StatusInternalServerError, wantKind: lebro.ModelErrorUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				if test.body != "" {
					_, _ = w.Write([]byte(test.body))
				}
			}))
			defer server.Close()

			embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
			if err != nil {
				t.Fatalf("NewEmbedder error = %v", err)
			}

			_, err = embedder.Embed(context.Background(), []string{"a"})
			var modelErr *lebro.ModelError
			assertModelErrorKind(t, err, test.wantKind, &modelErr)
			if modelErr.StatusCode != test.status {
				t.Fatalf("StatusCode = %d, want %d", modelErr.StatusCode, test.status)
			}
			if test.retryAfter != "" && modelErr.RetryAfter != 7*time.Second {
				t.Fatalf("RetryAfter = %v, want 7s", modelErr.RetryAfter)
			}
		})
	}
}

func TestEmbedderMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"a"})
	assertModelErrorKind(t, err, lebro.ModelErrorMalformedResponse, nil)
}

func TestEmbedderCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Cancellation must surface as the bare context error so a caller's
	// errors.Is check works uniformly across adapters.
	if _, err := embedder.Embed(ctx, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed error = %v, want context.Canceled", err)
	}
}

func TestEmbedderTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: url, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"a"})
	assertModelErrorKind(t, err, lebro.ModelErrorTransport, nil)
}

func TestEmbedderNilReceiverAndContext(t *testing.T) {
	var embedder *Embedder
	if _, err := embedder.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("Embed on nil embedder error = nil, want an error")
	}
	if got := embedder.Dimension(); got != 0 {
		t.Fatalf("Dimension() on nil embedder = %d, want 0", got)
	}

	real, err := NewEmbedder(EmbedderConfig{APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}
	//nolint:staticcheck // deliberately passing a nil context to assert the guard
	if _, err := real.Embed(nil, []string{"a"}); err == nil {
		t.Fatal("Embed with nil context error = nil, want an error")
	}
}

// TestEmbedderSatisfiesEmbeddingModel keeps the adapter usable wherever the
// neutral contract is expected.
func TestEmbedderSatisfiesEmbeddingModel(t *testing.T) {
	embedder, err := NewEmbedder(EmbedderConfig{APIKey: "k", Model: "m", Dimension: 8})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}
	var model lebro.EmbeddingModel = embedder
	if model.Dimension() != 8 {
		t.Fatalf("Dimension() = %d, want 8", model.Dimension())
	}
}

// assertModelErrorKind fails unless err is a *lebro.ModelError of the wanted
// kind, and returns it through out when the caller needs to inspect further
// fields. out may be nil.
func assertModelErrorKind(t *testing.T, err error, want lebro.ModelErrorKind, out **lebro.ModelError) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want a *lebro.ModelError of kind %q", want)
	}
	var modelErr *lebro.ModelError
	if !errors.As(err, &modelErr) {
		t.Fatalf("error = %v (%T), want a *lebro.ModelError", err, err)
	}
	if modelErr.Kind != want {
		t.Fatalf("Kind = %q, want %q", modelErr.Kind, want)
	}
	if modelErr.Provider != providerName {
		t.Fatalf("Provider = %q, want %q", modelErr.Provider, providerName)
	}
	if out != nil {
		*out = modelErr
	}
}

// TestEmbedderResponseErrorMatchesChatAdapter pins the delegation: both adapters
// must classify an HTTP error identically, so one retry policy covers both and
// a change to the chat mapping cannot silently skip embeddings.
func TestEmbedderResponseErrorMatchesChatAdapter(t *testing.T) {
	const body = `{"error":{"message":"slow down","type":"rate_limit_exceeded","code":"rate_limited","param":"input"}}`

	newServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(body))
		}))
	}

	embedServer := newServer()
	defer embedServer.Close()
	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: embedServer.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}
	_, embedErr := embedder.Embed(context.Background(), []string{"a"})

	chatServer := newServer()
	defer chatServer.Close()
	model, err := New(Config{BaseURL: chatServer.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	_, chatErr := model.Generate(context.Background(), lebro.ModelRequest{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})

	var embedModelErr, chatModelErr *lebro.ModelError
	if !errors.As(embedErr, &embedModelErr) {
		t.Fatalf("embed error = %v, want *lebro.ModelError", embedErr)
	}
	if !errors.As(chatErr, &chatModelErr) {
		t.Fatalf("chat error = %v, want *lebro.ModelError", chatErr)
	}

	if embedModelErr.Kind != chatModelErr.Kind {
		t.Fatalf("Kind: embeddings = %q, chat = %q", embedModelErr.Kind, chatModelErr.Kind)
	}
	if embedModelErr.Code != chatModelErr.Code {
		t.Fatalf("Code: embeddings = %q, chat = %q", embedModelErr.Code, chatModelErr.Code)
	}
	if embedModelErr.StatusCode != chatModelErr.StatusCode {
		t.Fatalf("StatusCode: embeddings = %d, chat = %d", embedModelErr.StatusCode, chatModelErr.StatusCode)
	}
	if embedModelErr.Message != chatModelErr.Message {
		t.Fatalf("Message: embeddings = %q, chat = %q", embedModelErr.Message, chatModelErr.Message)
	}
	if embedModelErr.RetryAfter != chatModelErr.RetryAfter {
		t.Fatalf("RetryAfter: embeddings = %v, chat = %v", embedModelErr.RetryAfter, chatModelErr.RetryAfter)
	}
	if string(embedModelErr.Extension) != string(chatModelErr.Extension) {
		t.Fatalf("Extension: embeddings = %s, chat = %s", embedModelErr.Extension, chatModelErr.Extension)
	}
}

// stalledErrorBodyURL serves one HTTP error response whose body is truncated: it
// declares a Content-Length larger than what it writes, then holds the
// connection open. A client therefore returns from Do with a usable response and
// an error status, and blocks inside the body read until the caller cancels.
//
// This is written against a raw listener rather than httptest because the
// pending read has to outlive Do. With an httptest handler that stalls before
// writing any body, net/http reports the cancellation from Do itself, which the
// transport classifier already handled — so such a test passes whether or not
// the response-error path is fixed, and proves nothing.
//
// cancel fires once the headers and partial body are on the wire.
func stalledErrorBodyURL(t *testing.T, cancel context.CancelFunc) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error = %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Drain the request so the client considers it sent.
		_, _ = conn.Read(make([]byte, 4096))
		// Promise 64 bytes of body and send 9, so the read stays pending.
		_, _ = conn.Write([]byte("HTTP/1.1 500 Internal Server Error\r\nContent-Length: 64\r\n\r\n{\"error\":"))
		// Let the client return from Do and enter the body read before
		// cancelling. Cancelling immediately would surface from Do instead, which
		// the transport classifier already handled.
		time.Sleep(50 * time.Millisecond)
		cancel()
		// Hold the connection until the test tears the listener down.
		<-done
	}()

	t.Cleanup(func() {
		_ = listener.Close()
	})
	return "http://" + listener.Addr().String()
}

// TestEmbedderCancelDuringErrorBodyRead asserts that a cancellation landing
// during the error-body read is reported as cancellation. The HTTP status is
// real, but the caller asked to stop, so reporting a retryable server error
// would both break errors.Is(err, context.Canceled) and invite a retry of a
// request nobody is waiting for.
//
// Verified to fail before the fix, where this returned kind=unavailable
// status=500 with errors.Is(err, context.Canceled) false.
func TestEmbedderCancelDuringErrorBodyRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := stalledErrorBodyURL(t, cancel)

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: baseURL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(ctx, []string{"a"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed error = %v, want context.Canceled", err)
	}
	// The HTTP status must not leak through as a retryable model error.
	var modelErr *lebro.ModelError
	if errors.As(err, &modelErr) {
		t.Fatalf("error = %v (kind %q), want bare cancellation", modelErr, modelErr.Kind)
	}
}

// TestModelCancelDuringErrorBodyRead is the same guarantee on the chat adapter,
// which shares the classifier.
func TestModelCancelDuringErrorBodyRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL := stalledErrorBodyURL(t, cancel)

	model, err := New(Config{BaseURL: baseURL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatalf("New error = %v", err)
	}

	_, err = model.Generate(ctx, lebro.ModelRequest{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate error = %v, want context.Canceled", err)
	}
	var modelErr *lebro.ModelError
	if errors.As(err, &modelErr) {
		t.Fatalf("error = %v (kind %q), want bare cancellation", modelErr, modelErr.Kind)
	}
}

// TestClassifyResponseErrorKeepsStatusWhenBodyReadSucceeds guards against
// over-correction: a normal HTTP error on a live context must still surface as
// the status error, not as cancellation.
func TestClassifyResponseErrorKeepsStatusWhenBodyReadSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer server.Close()

	embedder, err := NewEmbedder(EmbedderConfig{BaseURL: server.URL, APIKey: "k", Model: "m", Dimension: 2})
	if err != nil {
		t.Fatalf("NewEmbedder error = %v", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"a"})
	var modelErr *lebro.ModelError
	assertModelErrorKind(t, err, lebro.ModelErrorUnavailable, &modelErr)
	if modelErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("StatusCode = %d, want 500", modelErr.StatusCode)
	}
	if modelErr.Message != "boom" {
		t.Fatalf("Message = %q, want %q", modelErr.Message, "boom")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("a live-context HTTP error was misreported as cancellation")
	}
}
