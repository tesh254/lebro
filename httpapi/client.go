package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxResponseBodyBytes bounds a decoded non-streaming response body. A client
// pointed at a hostile or malfunctioning server should fail rather than
// allocate without limit. The bound is generous relative to any documented
// response and is not applied to the stream route, whose body is unbounded by
// design and consumed incrementally.
const maxResponseBodyBytes = 8 << 20

// ClientConfig configures a client for a lebro HTTP API.
type ClientConfig struct {
	// BaseURL is the root the server is mounted at, for example
	// "https://api.example.com" or "http://localhost:8080/lebro". A path
	// prefix is preserved, so a server mounted under a subpath is addressable.
	// Required.
	BaseURL string
	// HTTPClient performs the requests. When nil, a client with no timeout is
	// used: a run has no bound the SDK could pick correctly, so deadlines are
	// the caller's to set through the context. Supply your own client to
	// configure TLS, proxies, or connection pooling.
	//
	// A Timeout on this client applies to streamed runs too, where it bounds
	// the whole stream rather than one round trip — usually not what is wanted.
	// Prefer a context deadline for non-streaming calls and leave Timeout
	// unset when streaming.
	HTTPClient *http.Client
	// Header is called with every outgoing request before it is sent. Use it
	// for authentication, tracing, or tenancy headers; this package
	// deliberately implements no scheme, matching ServerConfig.Middleware on
	// the serving side.
	//
	// It must not modify the request's URL, method, or body: the client has
	// already built those from the call's arguments, and changing them would
	// send a request the caller did not make.
	Header func(*http.Request)
}

// Client calls a lebro HTTP API with the same result and stream contracts the
// in-process primitives use. Its methods mirror the routes a Server serves and
// return the same wire types the server produces, so a caller that moves an
// agent behind HTTP changes how it constructs the call, not how it reads the
// answer.
//
// Failures the server classifies are returned as *APIError, which unwraps to
// the matching lebro sentinel: errors.Is(err, lebro.ErrAgentToolFailure) holds
// for a remote tool failure exactly as it does for a local one.
//
// The zero value is not usable; construct one with NewClient. A Client is safe
// for concurrent use.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	header     func(*http.Request)
}

// NewClient creates a client for the server at config.BaseURL. It returns an
// error rather than panicking, unlike NewServer and mcp.NewClient, because a
// base URL is commonly read from configuration at runtime: a bad value is an
// operator's mistake to report, not necessarily a programming error.
func NewClient(config ClientConfig) (*Client, error) {
	if config.BaseURL == "" {
		return nil, errors.New("lebro/httpapi: BaseURL is required")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro/httpapi: parse BaseURL %q: %w", config.BaseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("lebro/httpapi: BaseURL %q must be http or https", config.BaseURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("lebro/httpapi: BaseURL %q has no host", config.BaseURL)
	}
	// Strip a trailing slash once here so joining a route path cannot produce a
	// doubled separator, which some servers route differently from the
	// single-slash form.
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL:    parsed,
		httpClient: httpClient,
		header:     config.Header,
	}, nil
}

// RunOption modifies a run request's query parameters.
type RunOption func(url.Values)

// WithThread binds the run to a durable thread. Prior messages in the thread
// are loaded before the run and the new transcript is appended on success. The
// server must be configured with a Store; naming a thread against a server
// without one is rejected as an invalid request rather than silently ignored.
//
// An empty id is a no-op, so a caller threading an optional value does not have
// to branch.
func WithThread(id string) RunOption {
	return func(query url.Values) {
		if id == "" {
			return
		}
		query.Set("thread_id", id)
	}
}

// MessageOption modifies a message listing's query parameters.
type MessageOption func(url.Values)

// WithCursor requests the page following a previous response's NextCursor. An
// empty cursor is a no-op, so a paging loop can pass the previous cursor
// unconditionally on its first iteration.
func WithCursor(cursor string) MessageOption {
	return func(query url.Values) {
		if cursor == "" {
			return
		}
		query.Set("cursor", cursor)
	}
}

// WithLimit caps how many messages a page returns. A non-positive limit is a
// no-op, letting the storage adapter choose, which is what the route does for
// an absent value.
func WithLimit(limit int) MessageOption {
	return func(query url.Values) {
		if limit <= 0 {
			return
		}
		query.Set("limit", strconv.Itoa(limit))
	}
}

// Health reports server readiness and what it exposes.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	return request[HealthResponse](ctx, c, http.MethodGet, "/health", nil, nil)
}

// ListAgents enumerates every agent the server exposes, in the server's stable
// ID order.
func (c *Client) ListAgents(ctx context.Context) ([]AgentSummary, error) {
	response, err := request[AgentListResponse](ctx, c, http.MethodGet, "/agents", nil, nil)
	if err != nil {
		return nil, err
	}
	return response.Agents, nil
}

// ListWorkflows enumerates every workflow the server exposes, including each
// one's declared input schema when it has one.
func (c *Client) ListWorkflows(ctx context.Context) ([]WorkflowSummary, error) {
	response, err := request[WorkflowListResponse](ctx, c, http.MethodGet, "/workflows", nil, nil)
	if err != nil {
		return nil, err
	}
	return response.Workflows, nil
}

// Run executes an agent to completion and returns its terminal result.
//
// The request supplies user text only; the agent's configured instructions
// remain the only system message, so a client cannot inject a system prompt.
// Cancelling ctx cancels the remote run: the server observes the closed
// connection and stops the run rather than finishing it unobserved.
func (c *Client) Run(ctx context.Context, agentID string, request_ RunRequest, options ...RunOption) (RunResponse, error) {
	if agentID == "" {
		return RunResponse{}, errors.New("lebro/httpapi: agent ID is required")
	}
	return request[RunResponse](ctx, c, http.MethodPost, "/agents/"+url.PathEscape(agentID)+"/runs", queryFrom(options), request_)
}

// RunWorkflow executes a workflow to completion and returns its final output.
// A run that suspends is returned with Status "suspended" and a non-nil
// Suspend describing where it stopped; resume is not available over HTTP, so
// the run cannot be continued through this client.
func (c *Client) RunWorkflow(ctx context.Context, workflowID string, request_ WorkflowRunRequest, options ...RunOption) (WorkflowRunResponse, error) {
	if workflowID == "" {
		return WorkflowRunResponse{}, errors.New("lebro/httpapi: workflow ID is required")
	}
	return request[WorkflowRunResponse](ctx, c, http.MethodPost, "/workflows/"+url.PathEscape(workflowID)+"/runs", queryFrom(options), request_)
}

// GetThread reads a durable conversation's metadata. It requires the server to
// be configured with a Store; without one the thread routes report not-found.
func (c *Client) GetThread(ctx context.Context, threadID string) (ThreadResponse, error) {
	if threadID == "" {
		return ThreadResponse{}, errors.New("lebro/httpapi: thread ID is required")
	}
	return request[ThreadResponse](ctx, c, http.MethodGet, "/threads/"+url.PathEscape(threadID), nil, nil)
}

// ListMessages returns one page of a thread's ordered messages. Follow
// NextCursor until it is empty to read the whole thread.
func (c *Client) ListMessages(ctx context.Context, threadID string, options ...MessageOption) (MessageListResponse, error) {
	if threadID == "" {
		return MessageListResponse{}, errors.New("lebro/httpapi: thread ID is required")
	}
	query := url.Values{}
	for _, option := range options {
		if option != nil {
			option(query)
		}
	}
	return request[MessageListResponse](ctx, c, http.MethodGet, "/threads/"+url.PathEscape(threadID)+"/messages", query, nil)
}

// OpenAPI fetches the server's generated OpenAPI document as raw JSON. It is
// returned unparsed because the document is a contract to hand to a generator
// or a validator, not a value this package models.
func (c *Client) OpenAPI(ctx context.Context) ([]byte, error) {
	response, err := c.do(ctx, http.MethodGet, "/openapi.json", nil, nil)
	if err != nil {
		return nil, err
	}
	defer closeBody(response)

	body, err := readBody(response)
	if err != nil {
		return nil, err
	}
	if err := errorForResponse(response, body); err != nil {
		return nil, err
	}
	return body, nil
}

// CheckCompatibility verifies that the server speaks a wire contract this
// client can read. It fetches the OpenAPI document and compares the major
// component of the server's contract version with ContractVersion, returning an
// error wrapping ErrIncompatibleContract when they differ.
//
// Only the major component is compared: a server on a later minor version
// serves fields this client ignores, which is precisely what a compatible
// addition means. A server that publishes no contract version predates the
// field and is reported as incompatible rather than assumed to match, because
// assuming would defeat the check on exactly the servers it exists for.
//
// It is not called automatically. A run must not pay a round trip it did not
// ask for, and a client whose server is known-good should not fail closed when
// the contract route is unavailable. Call it once at startup when the server's
// version is not otherwise pinned.
func (c *Client) CheckCompatibility(ctx context.Context) error {
	document, err := c.OpenAPI(ctx)
	if err != nil {
		return err
	}

	var parsed struct {
		Info map[string]json.RawMessage `json:"info"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		return fmt.Errorf("lebro/httpapi: decode OpenAPI document: %w", ErrMalformedResponse)
	}

	raw, ok := parsed.Info[contractVersionExtension]
	if !ok {
		return fmt.Errorf("lebro/httpapi: server publishes no contract version, client speaks %s: %w", ContractVersion, ErrIncompatibleContract)
	}
	var served string
	if err := json.Unmarshal(raw, &served); err != nil {
		return fmt.Errorf("lebro/httpapi: decode contract version: %w", ErrMalformedResponse)
	}
	if majorVersion(served) != majorVersion(ContractVersion) {
		return fmt.Errorf("lebro/httpapi: server contract %s, client contract %s: %w", served, ContractVersion, ErrIncompatibleContract)
	}
	return nil
}

// majorVersion returns the leading component of a dotted version string. A
// value with no dot is its own major version, and an empty string stays empty
// so it cannot accidentally equal a real version's major.
func majorVersion(version string) string {
	if index := strings.IndexByte(version, '.'); index >= 0 {
		return version[:index]
	}
	return version
}

// queryFrom applies run options to a fresh query. It returns nil when no option
// set anything, so a request with no options carries no query string at all
// rather than a bare "?".
func queryFrom(options []RunOption) url.Values {
	query := url.Values{}
	for _, option := range options {
		if option != nil {
			option(query)
		}
	}
	if len(query) == 0 {
		return nil
	}
	return query
}

// request performs a call and decodes its JSON response into T.
func request[T any](ctx context.Context, c *Client, method, path string, query url.Values, body any) (T, error) {
	var value T
	if c == nil {
		return value, errors.New("lebro/httpapi: client is nil")
	}

	response, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return value, err
	}
	defer closeBody(response)

	raw, err := readBody(response)
	if err != nil {
		return value, err
	}
	if err := errorForResponse(response, raw); err != nil {
		return value, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return value, fmt.Errorf("lebro/httpapi: empty response body: %w", ErrMalformedResponse)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, fmt.Errorf("lebro/httpapi: decode response: %w", ErrMalformedResponse)
	}
	return value, nil
}

// do builds and sends one request. The body is marshalled before the request is
// created so an encoding failure is reported without a round trip.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	if c == nil {
		return nil, errors.New("lebro/httpapi: client is nil")
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("lebro/httpapi: encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.resolve(path, query), reader)
	if err != nil {
		return nil, fmt.Errorf("lebro/httpapi: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.header != nil {
		c.header(request)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		// A cancelled context surfaces here as a transport error. Reporting it
		// as an APIError with ErrorCodeCancelled gives the caller the same
		// classification the server would have sent had it been able to
		// respond, and the APIError unwraps to context.Canceled so the
		// idiomatic check still works.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &APIError{Code: ErrorCodeCancelled, Message: publicMessage[ErrorCodeCancelled]}
		}
		return nil, fmt.Errorf("lebro/httpapi: %s %s: %w", method, path, err)
	}
	return response, nil
}

// resolve renders an absolute URL for a route path under the configured base.
func (c *Client) resolve(path string, query url.Values) string {
	target := *c.baseURL
	target.Path = c.baseURL.Path + path
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	return target.String()
}

// readBody reads a bounded response body.
func readBody(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("lebro/httpapi: read response body: %w", err)
	}
	if int64(len(body)) > maxResponseBodyBytes {
		return nil, fmt.Errorf("lebro/httpapi: response body exceeds %d bytes: %w", maxResponseBodyBytes, ErrMalformedResponse)
	}
	return body, nil
}

// errorForResponse returns an *APIError for a non-2xx response and nil
// otherwise. A body that does not decode to the documented error shape falls
// back to a status-derived classification, so a proxy's own error page is still
// reported as something the caller can branch on.
func errorForResponse(response *http.Response, body []byte) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	var decoded ErrorResponse
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Error.Code == "" {
		return apiErrorFromStatus(response.StatusCode)
	}
	return apiErrorFromBody(decoded.Error, response.StatusCode)
}

// closeBody drains and closes a response body so the connection can be reused.
// The drain is bounded: an unread body large enough to matter is not worth
// reading to reuse one connection.
func closeBody(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBodyBytes))
	_ = response.Body.Close()
}
