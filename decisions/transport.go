package decisions

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
	"time"

	"github.com/tesh254/lebro"
)

const (
	// maxErrorMessageLength bounds how much of a provider error description
	// is kept. Provider bodies are never echoed verbatim, so a validation
	// error cannot carry request content back into an error message.
	maxErrorMessageLength = 300
	// maxResponseBytes bounds how much of a provider response is read.
	maxResponseBytes = 8 << 20
	// defaultTimeout bounds one provider attempt when no timeout is
	// configured.
	defaultTimeout = 60 * time.Second
	// defaultRetryBackoff is the first backoff step between attempts.
	defaultRetryBackoff = 500 * time.Millisecond
	// defaultMaxRetries is the number of extra attempts a transient failure
	// gets.
	defaultMaxRetries = 2
	// defaultUserAgent identifies lebro's decision adapters.
	defaultUserAgent = "lebro-decisions"
)

// decisionClient performs the shared System One exchange.
type decisionClient struct {
	credential   string
	provider     string
	model        string
	endpoint     string
	authHeader   string
	authValue    string
	client       *http.Client
	timeout      time.Duration
	maxRetries   int
	retryBackoff time.Duration
	userAgent    string
}

// decisionResult is the outcome of one System One exchange.
type decisionResult struct {
	status     int
	body       []byte
	attempts   int
	duration   time.Duration
	requestID  string
	retryAfter time.Duration
}

// newDecisionClient builds the shared client for one provider.
func newDecisionClient(provider, baseURL, path, authHeader, authPrefix, apiKey, model string, config httpClientConfig) (*decisionClient, error) {
	if apiKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro: the base URL is not a valid URL: %w", errRedirectSafe(err))
	}
	if !parsed.IsAbs() {
		return nil, errors.New("lebro: the base URL must be absolute")
	}
	// The default timeout applies whether or not the caller supplied a
	// client: a client with no timeout of its own would otherwise let a
	// request hang until the caller's context ends.
	timeout := config.timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	client := config.client
	if client == nil {
		// The timeout is applied per attempt through the request context so
		// that a slow response cannot outlive it.
		client = &http.Client{}
	} else {
		// Copy the caller's client so its transport and settings are kept
		// without mutating a value the caller may share.
		copied := *client
		client = &copied
	}
	// A redirect is refused: a redirected request would carry the
	// credential header to another host.
	client.CheckRedirect = refuseRedirect
	maxRetries := defaultMaxRetries
	if config.maxRetries != nil {
		maxRetries = *config.maxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
	}
	backoff := config.retryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	return &decisionClient{
		credential:   apiKey,
		provider:     provider,
		model:        model,
		endpoint:     strings.TrimRight(baseURL, "/") + path,
		authHeader:   authHeader,
		authValue:    authPrefix + apiKey,
		client:       client,
		timeout:      timeout,
		maxRetries:   maxRetries,
		retryBackoff: backoff,
		userAgent:    defaultUserAgent,
	}, nil
}

// errRedirectSafe keeps a URL parse error from repeating the URL it failed
// on, because the URL can carry a credential in its userinfo or its query.
func errRedirectSafe(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.New(urlErr.Err.Error())
	}
	return err
}

// errRedirectRefused marks a redirect the adapter refused.
var errRedirectRefused = errors.New("lebro: the provider refused a redirect")

// refuseRedirect stops the adapter from following a redirect. A redirected
// request would forward the credential to the redirect target.
func refuseRedirect(*http.Request, []*http.Request) error {
	return errRedirectRefused
}

// httpClientConfig holds the transport knobs every provider shares.
type httpClientConfig struct {
	client       *http.Client
	timeout      time.Duration
	maxRetries   *int
	retryBackoff time.Duration
}

// defaultModel returns the model the adapter uses when a request does not
// name one. Each provider sets it when it is constructed.
func (c *decisionClient) defaultModel() string { return c.model }

// retryableStatuses turns a list of status codes into the set the client
// retries.
//
// The retry policy belongs to the model rather than to the adapter: two
// models of one provider can document different retryable statuses.
func retryableStatuses(statuses []int) map[int]bool {
	retryable := make(map[int]bool, len(statuses))
	for _, status := range statuses {
		retryable[status] = true
	}
	return retryable
}

// post sends the request, retrying the failures a provider documents as
// transient. It never retries across providers.
func (c *decisionClient) post(ctx context.Context, body []byte, requestIDHeaders []string, retryableStatus map[int]bool) (decisionResult, error) {
	started := time.Now()
	attempts := 0
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return decisionResult{attempts: attempts, duration: time.Since(started)}, c.cancellation(err)
		}
		attempts++
		status, payload, retryAfter, requestID, err := c.attempt(ctx, body, requestIDHeaders)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return decisionResult{attempts: attempts, duration: time.Since(started)}, c.cancellation(ctxErr)
			}
			if attempt < c.maxRetries && decisionErrorRetryable(err) {
				if waitErr := c.wait(ctx, c.backoff(attempt)); waitErr != nil {
					return decisionResult{attempts: attempts, duration: time.Since(started)}, waitErr
				}
				continue
			}
			return decisionResult{attempts: attempts, duration: time.Since(started)}, err
		}
		if status >= http.StatusBadRequest && retryableStatus[status] && attempt < c.maxRetries {
			delay := c.backoff(attempt)
			if retryAfter > delay {
				delay = retryAfter
			}
			if delay > c.timeout {
				// The provider asked for a wait longer than the adapter is
				// willing to sit through. Return the failure with the
				// delay so the caller can schedule the retry instead.
				return decisionResult{
					status:     status,
					body:       payload,
					attempts:   attempts,
					duration:   time.Since(started),
					retryAfter: retryAfter,
				}, nil
			}
			if waitErr := c.wait(ctx, delay); waitErr != nil {
				return decisionResult{attempts: attempts, duration: time.Since(started)}, waitErr
			}
			continue
		}
		return decisionResult{
			status:     status,
			body:       payload,
			attempts:   attempts,
			duration:   time.Since(started),
			requestID:  requestID,
			retryAfter: retryAfter,
		}, nil
	}
	return decisionResult{attempts: attempts, duration: time.Since(started)}, c.failure(lebro.DecisionErrorUnavailable, "lebro: the provider did not answer", nil)
}

// attempt performs one HTTP exchange.
func (c *decisionClient) attempt(ctx context.Context, body []byte, requestIDHeaders []string) (int, []byte, time.Duration, string, error) {
	requestCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, "", c.failure(lebro.DecisionErrorInvalidRequest, "lebro: build request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	if c.authHeader != "" {
		request.Header.Set(c.authHeader, c.authValue)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, nil, 0, "", c.transportFailure(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if readErr != nil {
		return 0, nil, 0, "", c.transportFailure(ctx, readErr)
	}
	requestID := ""
	for _, header := range requestIDHeaders {
		if value := response.Header.Get(header); value != "" {
			requestID = value
			break
		}
	}
	return response.StatusCode, payload, parseRetryAfter(response.Header.Get("Retry-After")), requestID, nil
}

// wait pauses between attempts and honors cancellation.
func (c *decisionClient) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return c.cancellation(ctx.Err())
	case <-timer.C:
		return nil
	}
}

// backoff returns the delay before the given attempt.
func (c *decisionClient) backoff(attempt int) time.Duration {
	delay := c.retryBackoff
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	return delay
}

// transportFailure classifies a failure that produced no HTTP response.
func (c *decisionClient) transportFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return c.cancellation(ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return c.failure(lebro.DecisionErrorTimeout, "lebro: the provider request timed out", err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return c.failure(lebro.DecisionErrorTimeout, "lebro: the provider request timed out", err)
	}
	if errors.Is(err, errRedirectRefused) {
		return c.failure(lebro.DecisionErrorTransport, "lebro: the provider refused a redirect", err)
	}
	// The transport error text is provider-controlled and can carry the
	// endpoint or the request, so the message stays fixed and only the
	// cause is retained.
	return c.failure(lebro.DecisionErrorTransport, "lebro: the provider request failed", err)
}

// cancellation reports a cancelled or expired caller context.
func (c *decisionClient) cancellation(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return c.failure(lebro.DecisionErrorTimeout, "lebro: the decision request deadline was exceeded", err)
	}
	return c.failure(lebro.DecisionErrorCancelled, "lebro: the decision request was cancelled", err)
}

// statusFailure maps an HTTP error response to a normalized failure.
//
// The provider's own description is kept, because it names the offending
// field, and it is redacted first: a provider can echo request content back
// in an error, and request content must never reach an error message.
func (c *decisionClient) statusFailure(status int, body []byte, retryAfter time.Duration, redactor *requestRedactor) error {
	message, code := extractProviderError(body)
	message = redactor.redact(message)
	code = redactor.redact(code)
	if message == "" {
		message = fmt.Sprintf("lebro: the provider returned HTTP %d", status)
	}
	return &lebro.DecisionError{
		Kind:       decisionStatusKind(status),
		Provider:   c.provider,
		Code:       code,
		StatusCode: status,
		RetryAfter: retryAfter,
		Message:    message,
	}
}

// requestRedactor removes request content from provider-supplied text.
//
// A provider can echo the request back in an error, and the request is the
// caller's content: the state, the criteria, an encoded image, or worse, the
// credential. The redactor replaces any such content with a marker before the
// text reaches an error message.
type requestRedactor struct {
	secrets []string
}

// minRedactedSecretLength is the shortest content the redactor treats as
// sensitive. A shorter value is too likely to appear in ordinary provider
// prose to be worth removing.
const minRedactedSecretLength = 8

// newRedactor collects the content that must not reach an error message.
func (c *decisionClient) newRedactor(request lebro.DecisionRequest, extra func(lebro.DecisionRequest) []string) *requestRedactor {
	redactor := &requestRedactor{}
	// The credential is always sensitive, whatever its length.
	if c.credential != "" {
		redactor.secrets = append(redactor.secrets, c.credential)
	}
	redactor.addContent(request.State)
	for _, question := range request.Questions {
		switch typed := question.(type) {
		case lebro.NoulQuestion:
			redactor.addContent(typed.Instructions)
			if typed.Criteria != nil {
				redactor.addContent(typed.Criteria.True)
				redactor.addContent(typed.Criteria.False)
			}
		case lebro.ChoiceQuestion:
			redactor.addContent(typed.Instructions)
			for _, description := range typed.Criteria {
				redactor.addContent(description)
			}
		case lebro.ScoreQuestion:
			redactor.addContent(typed.Instructions)
			for _, description := range typed.Criteria {
				redactor.addContent(description)
			}
		}
	}
	if extra != nil {
		for _, secret := range extra(request) {
			if secret != "" {
				redactor.secrets = append(redactor.secrets, secret)
			}
		}
	}
	return redactor
}

// addContent records one request value as sensitive.
func (r *requestRedactor) addContent(value lebro.JSONValue) {
	if value == nil {
		return
	}
	if text, ok := value.(string); ok {
		if len(strings.TrimSpace(text)) >= minRedactedSecretLength {
			r.secrets = append(r.secrets, text)
		}
		return
	}
	if encoded, err := json.Marshal(value); err == nil {
		if len(encoded) >= minRedactedSecretLength {
			r.secrets = append(r.secrets, string(encoded))
		}
	}
}

// redact removes every recorded secret from the text.
func (r *requestRedactor) redact(text string) string {
	if r == nil || text == "" {
		return text
	}
	for _, secret := range r.secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(text, secret) {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// failure builds a normalized failure.
func (c *decisionClient) failure(kind lebro.DecisionErrorKind, message string, err error) error {
	return &lebro.DecisionError{
		Kind:     kind,
		Provider: c.provider,
		Message:  message,
		Err:      err,
	}
}

// decisionErrorRetryable reports whether a failure is worth another attempt.
func decisionErrorRetryable(err error) bool {
	var decisionErr *lebro.DecisionError
	if errors.As(err, &decisionErr) {
		return decisionErr.Retryable()
	}
	return false
}

// decisionStatusKind maps an HTTP status to a normalized failure kind.
func decisionStatusKind(status int) lebro.DecisionErrorKind {
	switch status {
	case http.StatusUnauthorized:
		return lebro.DecisionErrorAuthentication
	case http.StatusPaymentRequired, http.StatusForbidden:
		return lebro.DecisionErrorPermissionDenied
	case http.StatusTooEarly:
		return lebro.DecisionErrorUnavailable
	case http.StatusTooManyRequests:
		return lebro.DecisionErrorRateLimited
	}
	if status >= http.StatusInternalServerError {
		return lebro.DecisionErrorUnavailable
	}
	return lebro.DecisionErrorInvalidRequest
}

// extractProviderError extracts a bounded description from a provider error
// body. Only documented message fields are read, so a validation error cannot
// echo request content such as an image back into an error message.
func extractProviderError(body []byte) (string, string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ""
	}
	if detail, ok := payload["detail"].(map[string]any); ok {
		message, _ := detail["message"].(string)
		code, _ := detail["error_type"].(string)
		return truncateErrorText(message), truncateErrorText(code)
	}
	if items, ok := payload["detail"].([]any); ok {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if message, ok := entry["msg"].(string); ok {
				parts = append(parts, message)
			}
		}
		return truncateErrorText(strings.Join(parts, "; ")), ""
	}
	if detail, ok := payload["detail"].(string); ok {
		return truncateErrorText(detail), ""
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		message, _ := nested["message"].(string)
		code, _ := nested["code"].(string)
		if code == "" {
			code, _ = nested["type"].(string)
		}
		return truncateErrorText(message), truncateErrorText(code)
	}
	if detail, ok := payload["error"].(string); ok {
		return truncateErrorText(detail), ""
	}
	message, _ := payload["message"].(string)
	code, _ := payload["code"].(string)
	return truncateErrorText(message), truncateErrorText(code)
}

// parseRetryAfter reads a Retry-After header value.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// truncateErrorText bounds a provider description.
func truncateErrorText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxErrorMessageLength {
		return text
	}
	return text[:maxErrorMessageLength] + "…"
}
