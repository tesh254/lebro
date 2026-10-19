package decisions

import (
	"bytes"
	"context"
	"encoding/base64"
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
	// systemOnePath is the System One evaluation path every provider in this
	// package exposes.
	systemOnePath = "/v1/systemone"
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

// systemOneProfile captures the documented differences between the providers
// this package adapts. Everything the providers share lives in the wire and
// client code; everything they document differently lives here.
type systemOneProfile struct {
	// provider is the stable provider identifier reported in a response.
	provider string
	// capabilities describes what the provider supports.
	capabilities lebro.DecisionCapabilities
	// scoreWeightedField is the wire field that holds the probability-
	// weighted level of a score answer.
	scoreWeightedField string
	// scoreWinnerField is the wire field that holds the winning level index
	// of a score answer. It is empty when the provider reports no winner.
	scoreWinnerField string
	// buildQuestions renders the question map. It is provider-specific
	// because one provider accepts text instructions only.
	buildQuestions func(map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error)
	// extraFields adds the provider-specific top-level request fields.
	extraFields func(lebro.DecisionRequest) map[string]any
	// imageFields renders the provider-specific image fields for a request.
	// It is nil when the provider takes no images.
	imageFields func(lebro.DecisionRequest) map[string]any
	// maxBodyBytes is the largest request body the provider accepts. Zero
	// means the provider documents no byte limit.
	maxBodyBytes int64
	// requestIDHeaders are the response headers that carry the provider
	// request identifier, when the provider documents one. The first header
	// that carries a value wins.
	requestIDHeaders []string
	// requestSecrets returns the provider-specific request content that must
	// never reach an error message, such as an encoded image payload.
	requestSecrets func(lebro.DecisionRequest) []string
}

// systemOneClient performs the shared System One exchange.
type systemOneClient struct {
	credential      string
	provider        string
	model           string
	endpoint        string
	authHeader      string
	authValue       string
	client          *http.Client
	timeout         time.Duration
	maxRetries      int
	retryBackoff    time.Duration
	retryableStatus map[int]bool
	userAgent       string
}

// systemOneResult is the outcome of one System One exchange.
type systemOneResult struct {
	status     int
	body       []byte
	attempts   int
	duration   time.Duration
	requestID  string
	retryAfter time.Duration
}

// newSystemOneClient builds the shared client for one provider.
func newSystemOneClient(provider, baseURL, path, authHeader, authPrefix, apiKey, model string, retryableStatuses []int, config httpClientConfig) (*systemOneClient, error) {
	if apiKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro: invalid base URL: %w", err)
	}
	if !parsed.IsAbs() {
		return nil, fmt.Errorf("lebro: base URL %q must be absolute", baseURL)
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
	retryable := make(map[int]bool, len(retryableStatuses))
	for _, status := range retryableStatuses {
		retryable[status] = true
	}
	return &systemOneClient{
		credential:      apiKey,
		provider:        provider,
		model:           model,
		endpoint:        strings.TrimRight(baseURL, "/") + path,
		authHeader:      authHeader,
		authValue:       authPrefix + apiKey,
		client:          client,
		timeout:         timeout,
		maxRetries:      maxRetries,
		retryBackoff:    backoff,
		retryableStatus: retryable,
		userAgent:       defaultUserAgent,
	}, nil
}

// refuseRedirect stops the adapter from following a redirect. A redirected
// request would forward the credential to the redirect target.
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("lebro: the provider refused a redirect")
}

// httpClientConfig holds the transport knobs every provider shares.
type httpClientConfig struct {
	client       *http.Client
	timeout      time.Duration
	maxRetries   *int
	retryBackoff time.Duration
}

// decide runs one decision request against a provider.
func decide(ctx context.Context, client *systemOneClient, profile systemOneProfile, request lebro.DecisionRequest) (lebro.DecisionResponse, error) {
	// The image capability gate runs first: a provider that does not accept
	// images at all reports that, rather than a complaint about the bytes.
	if err := lebro.ValidateDecisionImages(profile.capabilities.Images, request.Images); err != nil {
		kind := lebro.DecisionErrorInvalidRequest
		if errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
			kind = lebro.DecisionErrorUnsupportedCapability
		}
		return lebro.DecisionResponse{}, client.failure(kind, err.Error(), err)
	}
	if err := request.Validate(); err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}
	if err := checkSupportedKinds(profile, request); err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorUnsupportedCapability, err.Error(), err)
	}
	if err := checkQuestionLimits(profile, request); err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}

	model := request.Model
	if model == "" {
		model = client.defaultModel()
	}
	if model == "" {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, "lebro: model is required", nil)
	}

	questions, err := profile.buildQuestions(request.Questions)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, err.Error(), err)
	}

	body := map[string]any{
		"model":     model,
		"state":     request.State,
		"questions": questions,
	}
	if profile.extraFields != nil {
		for key, value := range profile.extraFields(request) {
			body[key] = value
		}
	}
	if profile.imageFields != nil {
		for key, value := range profile.imageFields(request) {
			body[key] = value
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, fmt.Sprintf("lebro: encode request: %v", err), err)
	}
	if profile.capabilities.MaxStateBytes > 0 {
		state, err := json.Marshal(request.State)
		if err != nil {
			return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest, fmt.Sprintf("lebro: encode state: %v", err), err)
		}
		if int64(len(state)) > profile.capabilities.MaxStateBytes {
			return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest,
				fmt.Sprintf("lebro: the state is %d bytes, %s accepts at most %d", len(state), profile.provider, profile.capabilities.MaxStateBytes), nil)
		}
	}
	if profile.maxBodyBytes > 0 && int64(len(encoded)) > profile.maxBodyBytes {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorInvalidRequest,
			fmt.Sprintf("lebro: the request body is %d bytes, %s accepts at most %d", len(encoded), profile.provider, profile.maxBodyBytes), nil)
	}

	result, err := client.post(ctx, encoded, profile.requestIDHeaders)
	if err != nil {
		return lebro.DecisionResponse{}, err
	}
	if result.status >= http.StatusBadRequest {
		// The redactor is built only here: a successful request must not
		// pay for a second copy of the request content.
		redactor := client.newRedactor(request, profile.requestSecrets)
		return lebro.DecisionResponse{}, client.statusFailure(result.status, result.body, result.retryAfter, redactor)
	}

	decoded, err := decodeSystemOneResponse(result.body)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, err.Error(), err)
	}
	if decoded.Model == "" {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the decision response carries no model identity", nil)
	}
	answers, err := mapSystemOneAnswers(request, decoded.Answers, profile.scoreWeightedField, profile.scoreWinnerField)
	if err != nil {
		return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, err.Error(), err)
	}

	response := lebro.DecisionResponse{
		Provider:  lebro.ProviderID(profile.provider),
		Model:     decoded.Model,
		Answers:   answers,
		RequestID: result.requestID,
		Duration:  result.duration,
		Attempts:  result.attempts,
	}
	if decoded.Usage != nil {
		usage := &lebro.DecisionUsage{}
		if decoded.Usage.InputTokens != nil {
			if *decoded.Usage.InputTokens < 0 {
				return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative input token count", nil)
			}
			tokens := *decoded.Usage.InputTokens
			usage.InputTokens = &tokens
		}
		if decoded.Usage.OutputTokens != nil {
			if *decoded.Usage.OutputTokens < 0 {
				return lebro.DecisionResponse{}, client.failure(lebro.DecisionErrorMalformedResponse, "lebro: the provider reported a negative output token count", nil)
			}
			tokens := *decoded.Usage.OutputTokens
			usage.OutputTokens = &tokens
		}
		response.Usage = usage
	}
	return response, nil
}

// checkSupportedKinds rejects a question kind the provider does not support.
func checkSupportedKinds(profile systemOneProfile, request lebro.DecisionRequest) error {
	for id, question := range request.Questions {
		switch question.(type) {
		case lebro.NoulQuestion, *lebro.NoulQuestion:
			if !profile.capabilities.Noul {
				return fmt.Errorf("lebro: decision question %q is a noul question, which %s does not support", id, profile.provider)
			}
		case lebro.ChoiceQuestion, *lebro.ChoiceQuestion:
			if !profile.capabilities.Choice {
				return fmt.Errorf("lebro: decision question %q is a choice question, which %s does not support", id, profile.provider)
			}
		case lebro.ScoreQuestion, *lebro.ScoreQuestion:
			if !profile.capabilities.Score {
				return fmt.Errorf("lebro: decision question %q is a score question, which %s does not support", id, profile.provider)
			}
		}
	}
	return nil
}

// checkQuestionLimits rejects a request that exceeds a documented limit.
func checkQuestionLimits(profile systemOneProfile, request lebro.DecisionRequest) error {
	caps := profile.capabilities
	if caps.MaxQuestions > 0 && len(request.Questions) > caps.MaxQuestions {
		return fmt.Errorf("lebro: %s accepts at most %d questions per request, got %d", profile.provider, caps.MaxQuestions, len(request.Questions))
	}
	if !caps.MultipleQuestions && len(request.Questions) > 1 {
		return fmt.Errorf("lebro: %s accepts one question per request, got %d", profile.provider, len(request.Questions))
	}
	for id, question := range request.Questions {
		var options, levels int
		switch typed := question.(type) {
		case lebro.ChoiceQuestion:
			options = len(typed.Criteria)
		case *lebro.ChoiceQuestion:
			if typed != nil {
				options = len(typed.Criteria)
			}
		case lebro.ScoreQuestion:
			levels = len(typed.Criteria)
		case *lebro.ScoreQuestion:
			if typed != nil {
				levels = len(typed.Criteria)
			}
		}
		if caps.MaxChoiceOptions > 0 && options > caps.MaxChoiceOptions {
			return fmt.Errorf("lebro: decision question %q has %d options, %s accepts at most %d", id, options, profile.provider, caps.MaxChoiceOptions)
		}
		if caps.MaxScoreLevels > 0 && levels > caps.MaxScoreLevels {
			return fmt.Errorf("lebro: decision question %q has %d levels, %s accepts at most %d", id, levels, profile.provider, caps.MaxScoreLevels)
		}
	}
	return nil
}

// defaultModel returns the model the adapter uses when a request does not
// name one. Each provider sets it when it is constructed.
func (c *systemOneClient) defaultModel() string { return c.model }

// post sends the request, retrying the failures a provider documents as
// transient. It never retries across providers.
func (c *systemOneClient) post(ctx context.Context, body []byte, requestIDHeaders []string) (systemOneResult, error) {
	started := time.Now()
	attempts := 0
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return systemOneResult{attempts: attempts, duration: time.Since(started)}, c.cancellation(err)
		}
		attempts++
		status, payload, retryAfter, requestID, err := c.attempt(ctx, body, requestIDHeaders)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return systemOneResult{attempts: attempts, duration: time.Since(started)}, c.cancellation(ctxErr)
			}
			if attempt < c.maxRetries && decisionErrorRetryable(err) {
				if waitErr := c.wait(ctx, c.backoff(attempt)); waitErr != nil {
					return systemOneResult{attempts: attempts, duration: time.Since(started)}, waitErr
				}
				continue
			}
			return systemOneResult{attempts: attempts, duration: time.Since(started)}, err
		}
		if status >= http.StatusBadRequest && c.retryableStatus[status] && attempt < c.maxRetries {
			delay := c.backoff(attempt)
			if retryAfter > delay {
				delay = retryAfter
			}
			if delay > c.timeout {
				// The provider asked for a wait longer than the adapter is
				// willing to sit through. Return the failure with the
				// delay so the caller can schedule the retry instead.
				return systemOneResult{
					status:     status,
					body:       payload,
					attempts:   attempts,
					duration:   time.Since(started),
					retryAfter: retryAfter,
				}, nil
			}
			if waitErr := c.wait(ctx, delay); waitErr != nil {
				return systemOneResult{attempts: attempts, duration: time.Since(started)}, waitErr
			}
			continue
		}
		return systemOneResult{
			status:     status,
			body:       payload,
			attempts:   attempts,
			duration:   time.Since(started),
			requestID:  requestID,
			retryAfter: retryAfter,
		}, nil
	}
	return systemOneResult{attempts: attempts, duration: time.Since(started)}, c.failure(lebro.DecisionErrorUnavailable, "lebro: the provider did not answer", nil)
}

// attempt performs one HTTP exchange.
func (c *systemOneClient) attempt(ctx context.Context, body []byte, requestIDHeaders []string) (int, []byte, time.Duration, string, error) {
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
func (c *systemOneClient) wait(ctx context.Context, delay time.Duration) error {
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
func (c *systemOneClient) backoff(attempt int) time.Duration {
	delay := c.retryBackoff
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	return delay
}

// transportFailure classifies a failure that produced no HTTP response.
func (c *systemOneClient) transportFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return c.cancellation(ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return c.failure(lebro.DecisionErrorTimeout, "lebro: the provider request timed out", err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		if urlErr.Timeout() {
			return c.failure(lebro.DecisionErrorTimeout, "lebro: the provider request timed out", err)
		}
		// The wrapped error text carries the endpoint URL. Keep it out of the
		// message so a request URL never reaches an error string.
		return c.failure(lebro.DecisionErrorTransport, "lebro: the provider request failed: "+urlErr.Err.Error(), err)
	}
	return c.failure(lebro.DecisionErrorTransport, "lebro: the provider request failed", err)
}

// cancellation reports a cancelled or expired caller context.
func (c *systemOneClient) cancellation(err error) error {
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
func (c *systemOneClient) statusFailure(status int, body []byte, retryAfter time.Duration, redactor *requestRedactor) error {
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
func (c *systemOneClient) newRedactor(request lebro.DecisionRequest, extra func(lebro.DecisionRequest) []string) *requestRedactor {
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
func (c *systemOneClient) failure(kind lebro.DecisionErrorKind, message string, err error) error {
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

// systemOneResponse is the shared System One response envelope.
type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   *systemOneUsage            `json:"usage"`
}

// systemOneUsage is the shared System One usage envelope.
type systemOneUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// systemOneAnswer is the shared System One answer shape. A provider fills
// the fields its question type defines and leaves the rest unset.
type systemOneAnswer struct {
	Type          string                     `json:"type"`
	Noul          *float64                   `json:"noul"`
	Choice        *string                    `json:"choice"`
	Probabilities map[string]float64         `json:"probabilities"`
	Confidence    *float64                   `json:"confidence"`
	Legend        map[string]json.RawMessage `json:"legend"`
	// Fields holds every field of the answer. A provider names its score
	// fields, so the answer cannot decode them into fixed fields.
	Fields map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps every field of the answer alongside the decoded ones.
func (a *systemOneAnswer) UnmarshalJSON(data []byte) error {
	type answerFields systemOneAnswer
	var decoded answerFields
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*a = systemOneAnswer(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	a.Fields = fields
	return nil
}

// number returns a numeric field of the answer.
func (a systemOneAnswer) number(name string) (json.Number, bool) {
	raw, ok := a.Fields[name]
	if !ok {
		return "", false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", false
	}
	return number, true
}

// decodeSystemOneResponse decodes the shared response envelope.
func decodeSystemOneResponse(body []byte) (*systemOneResponse, error) {
	var decoded systemOneResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("lebro: decode the decision response: %w", err)
	}
	if decoded.Answers == nil {
		return nil, errors.New("lebro: the decision response carries no answers")
	}
	return &decoded, nil
}

// mapSystemOneAnswers maps the provider answers onto the requested question
// identifiers.
func mapSystemOneAnswers(request lebro.DecisionRequest, wire map[string]systemOneAnswer, weightedField, winnerField string) (map[lebro.DecisionQuestionID]lebro.DecisionAnswer, error) {
	answers := make(map[lebro.DecisionQuestionID]lebro.DecisionAnswer, len(request.Questions))
	for id, question := range request.Questions {
		raw, ok := wire[string(id)]
		if !ok {
			return nil, fmt.Errorf("lebro: the provider returned no answer for question %q", id)
		}
		answer, err := mapSystemOneAnswer(id, question, raw, weightedField, winnerField)
		if err != nil {
			return nil, err
		}
		answers[id] = answer
	}
	return answers, nil
}

// mapSystemOneAnswer maps one provider answer onto the neutral contract.
func mapSystemOneAnswer(id lebro.DecisionQuestionID, question lebro.DecisionQuestion, raw systemOneAnswer, weightedField, winnerField string) (lebro.DecisionAnswer, error) {
	switch typed := question.(type) {
	case lebro.NoulQuestion:
		return mapSystemOneNoul(id, raw)
	case *lebro.NoulQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapSystemOneNoul(id, raw)
	case lebro.ChoiceQuestion:
		return mapSystemOneChoice(id, typed, raw)
	case *lebro.ChoiceQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapSystemOneChoice(id, *typed, raw)
	case lebro.ScoreQuestion:
		return mapSystemOneScore(id, typed, raw, weightedField, winnerField)
	case *lebro.ScoreQuestion:
		if typed == nil {
			return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
		}
		return mapSystemOneScore(id, *typed, raw, weightedField, winnerField)
	default:
		return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
	}
}

// mapSystemOneNoul maps a noul answer. The provider confidence is reported
// only when the provider sends one.
func mapSystemOneNoul(id lebro.DecisionQuestionID, raw systemOneAnswer) (lebro.DecisionAnswer, error) {
	if raw.Type != "noul" {
		return nil, fmt.Errorf("lebro: decision question %q expected a noul answer, got %q", id, raw.Type)
	}
	if raw.Noul == nil {
		return nil, fmt.Errorf("lebro: decision question %q returned no noul probability", id)
	}
	if err := checkProbability(*raw.Noul); err != nil {
		return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
	}
	answer := lebro.NoulAnswer{ProbabilityYes: *raw.Noul}
	if raw.Confidence != nil {
		if err := checkProbability(*raw.Confidence); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		confidence := *raw.Confidence
		answer.Confidence = &confidence
	}
	return answer, nil
}

// mapSystemOneChoice maps a choice answer.
func mapSystemOneChoice(id lebro.DecisionQuestionID, question lebro.ChoiceQuestion, raw systemOneAnswer) (lebro.DecisionAnswer, error) {
	if raw.Type != "choice" {
		return nil, fmt.Errorf("lebro: decision question %q expected a choice answer, got %q", id, raw.Type)
	}
	if raw.Choice == nil || *raw.Choice == "" {
		return nil, fmt.Errorf("lebro: decision question %q returned no selected option", id)
	}
	selected := lebro.DecisionOptionID(*raw.Choice)
	if _, ok := question.Criteria[selected]; !ok {
		return nil, fmt.Errorf("lebro: decision question %q selected option %q, which was not requested", id, selected)
	}
	probabilities, err := mapSystemOneProbabilities(id, question.Criteria, raw.Probabilities)
	if err != nil {
		return nil, err
	}
	answer := lebro.ChoiceAnswer{Choice: selected, Probabilities: probabilities}
	if raw.Confidence != nil {
		if err := checkProbability(*raw.Confidence); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		confidence := *raw.Confidence
		answer.Confidence = &confidence
	}
	return answer, nil
}

// mapSystemOneProbabilities maps the provider distribution onto the requested
// options. Every requested option must carry a probability: a partial
// distribution is a malformed response, not a distribution with implicit
// zeros.
func mapSystemOneProbabilities(id lebro.DecisionQuestionID, requested map[lebro.DecisionOptionID]lebro.JSONValue, reported map[string]float64) (map[lebro.DecisionOptionID]float64, error) {
	if len(requested) == 0 {
		return nil, fmt.Errorf("lebro: decision question %q requested no options", id)
	}
	for option := range reported {
		if _, ok := requested[lebro.DecisionOptionID(option)]; !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned a probability for option %q, which was not requested", id, option)
		}
	}
	probabilities := make(map[lebro.DecisionOptionID]float64, len(requested))
	for option := range requested {
		probability, ok := reported[string(option)]
		if !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned no probability for option %q", id, option)
		}
		if err := checkProbability(probability); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		probabilities[option] = probability
	}
	return probabilities, nil
}

// mapSystemOneScore maps a score answer and keeps a selected level separate
// from a probability-weighted value.
func mapSystemOneScore(id lebro.DecisionQuestionID, question lebro.ScoreQuestion, raw systemOneAnswer, weightedField, winnerField string) (lebro.DecisionAnswer, error) {
	if raw.Type != "score" {
		return nil, fmt.Errorf("lebro: decision question %q expected a score answer, got %q", id, raw.Type)
	}
	probabilities, err := mapSystemOneLevelProbabilities(id, len(question.Criteria), raw.Probabilities)
	if err != nil {
		return nil, err
	}
	legend, err := mapSystemOneLegend(id, len(question.Criteria), raw.Legend)
	if err != nil {
		return nil, err
	}
	answer := lebro.ScoreAnswer{
		Probabilities: probabilities,
		Legend:        legend,
		Provenance:    lebro.ScoreNative,
	}
	// The provider names its score fields, so the adapter reads the fields
	// the configuration names and requires the ones the provider must send.
	weighted, ok := raw.number(weightedField)
	if !ok {
		return nil, fmt.Errorf("lebro: decision question %q returned no %q score field", id, weightedField)
	}
	expected, err := weighted.Float64()
	if err != nil {
		return nil, fmt.Errorf("lebro: decision question %q returned a %q score that is not a number", id, weightedField)
	}
	answer.ExpectedLevel = &expected
	if winnerField != "" {
		winner, ok := raw.number(winnerField)
		if !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned no %q winning level", id, winnerField)
		}
		level, err := strconv.Atoi(winner.String())
		if err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a %q level that is not an integer", id, winnerField)
		}
		if level < 0 || level >= len(question.Criteria) {
			return nil, fmt.Errorf("lebro: decision question %q returned score level %d, outside the requested range", id, level)
		}
		answer.SelectedLevel = &level
	}
	if raw.Confidence != nil {
		if err := checkProbability(*raw.Confidence); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		confidence := *raw.Confidence
		answer.Confidence = &confidence
	}
	return answer, nil
}

// mapSystemOneLevelProbabilities maps the provider level distribution onto
// the requested level indexes. A level the caller did not ask for is a
// malformed response, not a level to ignore.
func mapSystemOneLevelProbabilities(id lebro.DecisionQuestionID, levels int, reported map[string]float64) (map[int]float64, error) {
	if levels <= 0 {
		return nil, fmt.Errorf("lebro: decision question %q requested no levels", id)
	}
	for key := range reported {
		level, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a probability key that is not a level index", id)
		}
		if level < 0 || level >= levels {
			return nil, fmt.Errorf("lebro: decision question %q returned probability for level %d, outside the requested range", id, level)
		}
	}
	probabilities := make(map[int]float64, levels)
	for level := 0; level < levels; level++ {
		probability, ok := reported[strconv.Itoa(level)]
		if !ok {
			return nil, fmt.Errorf("lebro: decision question %q returned no probability for level %d", id, level)
		}
		if err := checkProbability(probability); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q: %w", id, err)
		}
		probabilities[level] = probability
	}
	return probabilities, nil
}

// mapSystemOneLegend maps the provider legend onto the requested level
// indexes.
func mapSystemOneLegend(id lebro.DecisionQuestionID, levels int, raw map[string]json.RawMessage) (map[int]lebro.JSONValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	legend := make(map[int]lebro.JSONValue, len(raw))
	for key, value := range raw {
		level, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a legend key that is not a level index", id)
		}
		if level < 0 || level >= levels {
			return nil, fmt.Errorf("lebro: decision question %q returned legend level %d, outside the requested range", id, level)
		}
		var decoded lebro.JSONValue
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, fmt.Errorf("lebro: decision question %q returned a legend entry that is not valid JSON", id)
		}
		legend[level] = decoded
	}
	return legend, nil
}

// checkProbability rejects a probability outside the unit interval.
func checkProbability(probability float64) error {
	if probability < 0 || probability > 1 {
		return fmt.Errorf("the provider returned the probability %v, which is outside the range 0 to 1", probability)
	}
	return nil
}

// buildSystemOneQuestions renders the shared question map. Instructions and
// criteria keep their JSON shape.
func buildSystemOneQuestions(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
	rendered := make(map[string]any, len(questions))
	for id, question := range questions {
		typedNoul, isNoul := question.(lebro.NoulQuestion)
		if pointer, ok := question.(*lebro.NoulQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedNoul, isNoul = *pointer, true
		}
		typedChoice, isChoice := question.(lebro.ChoiceQuestion)
		if pointer, ok := question.(*lebro.ChoiceQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedChoice, isChoice = *pointer, true
		}
		typedScore, isScore := question.(lebro.ScoreQuestion)
		if pointer, ok := question.(*lebro.ScoreQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedScore, isScore = *pointer, true
		}
		switch {
		case isNoul:
			typed := typedNoul
			wire := map[string]any{
				"type":         "noul",
				"instructions": typed.Instructions,
			}
			if typed.Criteria != nil {
				criteria := map[string]any{}
				if typed.Criteria.True != nil {
					criteria["true"] = typed.Criteria.True
				}
				if typed.Criteria.False != nil {
					criteria["false"] = typed.Criteria.False
				}
				if len(criteria) > 0 {
					wire["criteria"] = criteria
				}
			}
			rendered[string(id)] = wire
		case isChoice:
			typed := typedChoice
			criteria := make(map[string]any, len(typed.Criteria))
			for option, description := range typed.Criteria {
				criteria[string(option)] = description
			}
			rendered[string(id)] = map[string]any{
				"type":         "choice",
				"instructions": typed.Instructions,
				"criteria":     criteria,
			}
		case isScore:
			typed := typedScore
			criteria := make([]any, 0, len(typed.Criteria))
			for _, description := range typed.Criteria {
				criteria = append(criteria, description)
			}
			rendered[string(id)] = map[string]any{
				"type":         "score",
				"instructions": typed.Instructions,
				"criteria":     criteria,
			}
		default:
			return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
		}
	}
	return rendered, nil
}

// buildTextOnlyQuestions renders the question map for a provider that accepts
// text instructions and text criteria only. A structured value is rejected
// before the request is sent rather than flattened into something the
// provider never described.
func buildTextOnlyQuestions(questions map[lebro.DecisionQuestionID]lebro.DecisionQuestion) (map[string]any, error) {
	rendered := make(map[string]any, len(questions))
	for id, question := range questions {
		typedNoul, isNoul := question.(lebro.NoulQuestion)
		if pointer, ok := question.(*lebro.NoulQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedNoul, isNoul = *pointer, true
		}
		typedChoice, isChoice := question.(lebro.ChoiceQuestion)
		if pointer, ok := question.(*lebro.ChoiceQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedChoice, isChoice = *pointer, true
		}
		typedScore, isScore := question.(lebro.ScoreQuestion)
		if pointer, ok := question.(*lebro.ScoreQuestion); ok {
			if pointer == nil {
				return nil, fmt.Errorf("lebro: decision question %q must not be nil", id)
			}
			typedScore, isScore = *pointer, true
		}
		switch {
		case isNoul:
			typed := typedNoul
			instructions, err := textInstruction(id, typed.Instructions)
			if err != nil {
				return nil, err
			}
			wire := map[string]any{
				"type":         "noul",
				"instructions": instructions,
			}
			if typed.Criteria != nil {
				criteria := map[string]any{}
				if typed.Criteria.True != nil {
					text, err := textCriterion(id, "true", typed.Criteria.True)
					if err != nil {
						return nil, err
					}
					criteria["true"] = text
				}
				if typed.Criteria.False != nil {
					text, err := textCriterion(id, "false", typed.Criteria.False)
					if err != nil {
						return nil, err
					}
					criteria["false"] = text
				}
				if len(criteria) > 0 {
					wire["criteria"] = criteria
				}
			}
			rendered[string(id)] = wire
		case isChoice:
			typed := typedChoice
			instructions, err := textInstruction(id, typed.Instructions)
			if err != nil {
				return nil, err
			}
			criteria := make(map[string]any, len(typed.Criteria))
			for option, description := range typed.Criteria {
				text, err := textCriterion(id, string(option), description)
				if err != nil {
					return nil, err
				}
				criteria[string(option)] = text
			}
			rendered[string(id)] = map[string]any{
				"type":         "choice",
				"instructions": instructions,
				"criteria":     criteria,
			}
		case isScore:
			typed := typedScore
			instructions, err := textInstruction(id, typed.Instructions)
			if err != nil {
				return nil, err
			}
			criteria := make([]any, 0, len(typed.Criteria))
			for level, description := range typed.Criteria {
				text, err := textCriterion(id, strconv.Itoa(level), description)
				if err != nil {
					return nil, err
				}
				criteria = append(criteria, text)
			}
			rendered[string(id)] = map[string]any{
				"type":         "score",
				"instructions": instructions,
				"criteria":     criteria,
			}
		default:
			return nil, fmt.Errorf("lebro: decision question %q has an unsupported type %T", id, question)
		}
	}
	return rendered, nil
}

// textInstruction requires a non-empty text instruction.
func textInstruction(id lebro.DecisionQuestionID, instructions lebro.JSONValue) (string, error) {
	text, ok := instructions.(string)
	if !ok {
		return "", fmt.Errorf("lebro: decision question %q requires text instructions, and the provider accepts text only", id)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("lebro: decision question %q requires non-empty instructions", id)
	}
	return text, nil
}

// textCriterion requires a text criterion.
func textCriterion(id lebro.DecisionQuestionID, name string, criterion lebro.JSONValue) (string, error) {
	text, ok := criterion.(string)
	if !ok {
		return "", fmt.Errorf("lebro: decision question %q requires text criteria, and the provider accepts text only", id)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("lebro: decision question %q requires non-empty criteria", id)
	}
	return text, nil
}

// encodeImageBase64 encodes image bytes for a provider wire format. The
// encoding happens here, at the wire boundary, and the result is never
// retained.
func encodeImageBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
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
