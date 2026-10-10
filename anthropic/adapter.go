// Package anthropic adapts Anthropic's Messages API to lebro.Model.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	claude "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/tesh254/lebro"
)

const (
	providerName                 = "anthropic"
	openRouterProviderName       = "openrouter"
	defaultBaseURL               = "https://api.anthropic.com"
	defaultMaxTokens       int64 = 4096
)

// Config configures an adapter for the Anthropic Messages API or any endpoint
// that speaks it, such as OpenRouter's Anthropic-compatible API or a gateway.
// Exactly one of APIKey or AuthToken is required. The adapter reads nothing
// from the environment: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, and
// ANTHROPIC_BASE_URL are ignored so the configured endpoint and credential are
// the only ones a request can use.
type Config struct {
	// APIKey is sent in the X-Api-Key header, as the Anthropic API expects.
	APIKey string
	// AuthToken is sent as an Authorization Bearer token, as OpenRouter and
	// many Anthropic-compatible gateways expect.
	AuthToken string
	// Model is the default model id used when a request omits
	// ModelRequest.Model. Any id the endpoint accepts is allowed.
	Model string
	// BaseURL is the API root, for example "https://api.anthropic.com" or
	// "https://openrouter.ai/api". It defaults to the Anthropic API.
	BaseURL string
	// Headers are sent on every request, for example anthropic-beta flags,
	// an anthropic-version override, or gateway attribution headers. They
	// cannot replace the credential headers.
	Headers map[string]string
	// HTTPClient issues requests. Its Timeout, when set, also bounds streams.
	HTTPClient *http.Client
	// Timeout caps each non-streaming request. Zero lets the SDK derive a
	// limit from MaxTokens and reject non-streaming requests that would
	// likely exceed ten minutes. Streams are bounded by the caller context.
	Timeout time.Duration
	// MaxRetries overrides the SDK's retry count for retryable failures.
	// Nil keeps the SDK default; zero disables retries so a router or the
	// caller owns retry policy.
	MaxRetries *int
	// MaxTokens is the default output-token limit. It defaults to 4096.
	MaxTokens int64
	// ProviderID labels attempts, errors, and metrics. It defaults to
	// "openrouter" for openrouter.ai and "anthropic" otherwise.
	ProviderID lebro.ProviderID
	// PricingDomain identifies the billing contract behind the endpoint. It
	// defaults from the BaseURL host: the Anthropic API, OpenRouter, or the
	// unpriced anthropic_compatible domain for any other gateway, so a proxy
	// is never priced as the Anthropic API by accident.
	PricingDomain lebro.PricingDomain
}

// Model implements lebro.Model and lebro.StreamingModel.
type Model struct {
	client        *claude.Client
	model         string
	maxTokens     int64
	timeout       time.Duration
	providerID    lebro.ProviderID
	pricingDomain lebro.PricingDomain
}

var _ lebro.Model = (*Model)(nil)
var _ lebro.StreamingModel = (*Model)(nil)

func (m *Model) ProviderID() lebro.ProviderID {
	if m == nil || m.providerID == "" {
		return providerName
	}
	return m.providerID
}

// New creates an Anthropic Messages adapter safe for concurrent use.
func New(config Config) (*Model, error) {
	if (config.APIKey == "") == (config.AuthToken == "") {
		return nil, errors.New("lebro: exactly one of API key or auth token is required")
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("lebro: invalid base URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("lebro: base URL %q must be an absolute http or https URL", baseURL)
	}
	maxTokens := config.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	if maxTokens < 1 {
		return nil, errors.New("lebro: max tokens must be positive")
	}
	if config.Timeout < 0 {
		return nil, errors.New("lebro: timeout must not be negative")
	}
	if config.MaxRetries != nil && *config.MaxRetries < 0 {
		return nil, errors.New("lebro: max retries must not be negative")
	}

	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithBaseURL(baseURL)}
	for key, value := range config.Headers {
		switch strings.ToLower(key) {
		case "x-api-key", "authorization":
			return nil, fmt.Errorf("lebro: header %q is set from APIKey or AuthToken", key)
		}
		opts = append(opts, option.WithHeader(key, value))
	}
	if config.APIKey != "" {
		opts = append(opts, option.WithAPIKey(config.APIKey))
	} else {
		opts = append(opts, option.WithAuthToken(config.AuthToken))
	}
	if config.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(config.HTTPClient))
	}
	if config.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*config.MaxRetries))
	}

	host := strings.ToLower(parsed.Hostname())
	providerID := config.ProviderID
	if providerID == "" {
		providerID = providerName
		if host == "openrouter.ai" {
			providerID = openRouterProviderName
		}
	}
	pricingDomain := config.PricingDomain
	if pricingDomain == "" {
		switch host {
		case "api.anthropic.com":
			pricingDomain = lebro.PricingDomainAnthropic
		case "openrouter.ai":
			pricingDomain = lebro.PricingDomainOpenRouter
		default:
			pricingDomain = lebro.PricingDomainAnthropicCompatible
		}
	}
	client := claude.NewClient(opts...)
	return &Model{client: &client, model: config.Model, maxTokens: maxTokens, timeout: config.Timeout, providerID: providerID, pricingDomain: pricingDomain}, nil
}

func (m *Model) Generate(ctx context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	params, err := m.params(request)
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	var opts []option.RequestOption
	if m.timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(m.timeout))
	}
	// The SDK refuses non-streaming requests it expects to outlast its
	// default timeout. Check first so that refusal is an invalid request a
	// router will not retry, rather than an opaque unavailable error.
	if _, err := claude.CalculateNonStreamingTimeout(int(params.MaxTokens), params.Model, opts); err != nil {
		return lebro.ModelResponse{}, m.invalid(fmt.Errorf("lebro: %w; use Stream or set Config.Timeout", err))
	}
	response, err := m.client.Messages.New(ctx, params, opts...)
	if err != nil {
		return lebro.ModelResponse{}, m.error(ctx, err)
	}
	return m.response(request, response)
}

// Stream delivers native Messages API text chunks and complete tool calls. It
// deliberately passes the caller context directly to the SDK: this adapter
// adds no total-request timeout, so stream lifetime is controlled by the
// caller context and any timeout configured on the supplied HTTP client.
func (m *Model) Stream(ctx context.Context, request lebro.ModelRequest) (lebro.StreamReader, error) {
	params, err := m.params(request)
	if err != nil {
		return nil, err
	}
	stream := m.client.Messages.NewStreaming(ctx, params)
	reader := &anthropicStream{stream: stream, values: make(chan lebro.StreamDelta, 8), done: make(chan struct{}), errorFn: m.error, provider: string(m.ProviderID()), pricingDomain: m.pricingDomain}
	reader.start(ctx, request)
	return reader, nil
}

func (m *Model) params(request lebro.ModelRequest) (claude.MessageNewParams, error) {
	if err := request.Validate(); err != nil {
		return claude.MessageNewParams{}, m.invalid(err)
	}
	model := request.Model
	if model == "" {
		model = m.model
	}
	if model == "" {
		return claude.MessageNewParams{}, m.invalid(errors.New("lebro: model is required"))
	}
	params := claude.MessageNewParams{Model: claude.Model(model), MaxTokens: m.maxTokens}
	if request.MaxOutputTokens > 0 {
		params.MaxTokens = request.MaxOutputTokens
	}
	replayThinking := false
	if thinking, err := m.reasoningParams(request.Reasoning, params.MaxTokens); err != nil {
		return claude.MessageNewParams{}, m.invalid(err)
	} else if thinking != nil {
		params.Thinking = *thinking
		replayThinking = thinking.OfEnabled != nil
	}
	for _, message := range request.Messages {
		switch message.Role {
		case lebro.RoleSystem:
			params.System = append(params.System, claude.TextBlockParam{Text: message.Content})
		case lebro.RoleUser:
			blocks, err := anthropicUserBlocks(message)
			if err != nil {
				return claude.MessageNewParams{}, m.invalid(err)
			}
			params.Messages = append(params.Messages, claude.NewUserMessage(blocks...))
		case lebro.RoleAssistant:
			blocks := []claude.ContentBlockParamUnion{}
			if replayThinking {
				thinking, err := anthropicReasoningBlocks(message.Reasoning)
				if err != nil {
					return claude.MessageNewParams{}, m.invalid(err)
				}
				blocks = append(blocks, thinking...)
			}
			if message.Content != "" {
				blocks = append(blocks, claude.NewTextBlock(message.Content))
			}
			for _, call := range message.ToolCalls.Values() {
				var input any
				if err := json.Unmarshal(call.Arguments, &input); err != nil {
					return claude.MessageNewParams{}, m.invalid(err)
				}
				blocks = append(blocks, claude.NewToolUseBlock(call.ID, input, string(call.ToolID)))
			}
			params.Messages = append(params.Messages, claude.NewAssistantMessage(blocks...))
		case lebro.RoleTool:
			params.Messages = append(params.Messages, claude.NewUserMessage(claude.NewToolResultBlock(message.ToolCallID, message.Content, false)))
		}
	}
	for _, tool := range request.Tools {
		schema := map[string]any{}
		if len(tool.InputSchema) > 0 {
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				return claude.MessageNewParams{}, m.invalid(err)
			}
		}
		params.Tools = append(params.Tools, claude.ToolUnionParam{OfTool: &claude.ToolParam{
			Name: string(tool.ID), Description: claude.String(tool.Description),
			InputSchema: claude.ToolInputSchemaParam{ExtraFields: schema},
		}})
	}
	if request.OutputSchema != nil {
		var schema map[string]any
		if err := json.Unmarshal(request.OutputSchema.Schema, &schema); err != nil {
			return claude.MessageNewParams{}, m.invalid(err)
		}
		params.OutputConfig = claude.OutputConfigParam{Format: claude.JSONOutputFormatParam{Schema: schema}}
	}
	return params, nil
}

// anthropicUserImageMediaTypes is the static, documented set of image media
// types the Messages API accepts for base64 image sources. Requests carrying
// any other image type fail locally with a clear message instead of traveling
// to the endpoint only to be rejected.
var anthropicUserImageMediaTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/gif":  {},
	"image/webp": {},
}

// anthropicUserBlocks maps a user message onto ordered Messages API content
// blocks, preserving part order. Images become base64 image sources and PDF
// documents become base64 document sources; the document filename is preserved
// as the block title because the API has no filename field. Unknown part kinds
// fail the request instead of silently dropping content.
func anthropicUserBlocks(message lebro.Message) ([]claude.ContentBlockParamUnion, error) {
	parts := message.ContentParts.Values()
	if len(parts) == 0 {
		return []claude.ContentBlockParamUnion{claude.NewTextBlock(message.Content)}, nil
	}
	blocks := make([]claude.ContentBlockParamUnion, 0, len(parts))
	for i, part := range parts {
		switch part.Type {
		case lebro.ContentPartText:
			blocks = append(blocks, claude.NewTextBlock(part.Text))
		case lebro.ContentPartImage:
			// Media types are case-insensitive; Anthropic's documented set is
			// lowercase, so normalize for the capability check and the wire
			// value while the error reports what the caller sent.
			mediaType := strings.ToLower(part.MimeType)
			if _, ok := anthropicUserImageMediaTypes[mediaType]; !ok {
				return nil, fmt.Errorf("lebro: Anthropic image content part %d media type %q is not supported; use image/jpeg, image/png, image/gif, or image/webp", i, part.MimeType)
			}
			blocks = append(blocks, claude.NewImageBlock(claude.Base64ImageSourceParam{
				Data:      part.Data,
				MediaType: claude.Base64ImageSourceMediaType(mediaType),
			}))
		case lebro.ContentPartDocument:
			blocks = append(blocks, claude.ContentBlockParamUnion{OfDocument: &claude.DocumentBlockParam{
				Source: claude.DocumentBlockParamSourceUnion{OfBase64: &claude.Base64PDFSourceParam{Data: part.Data}},
				Title:  claude.String(part.Filename),
			}})
		default:
			return nil, fmt.Errorf("lebro: message content part %d has unsupported type %q", i, part.Type)
		}
	}
	return blocks, nil
}

// reasoningParams maps neutral effort to Anthropic's token-budget mechanism.
// Every enabled budget must be at least 1024 and strictly below max_tokens.
func (m *Model) reasoningParams(config lebro.ReasoningConfig, maxTokens int64) (*claude.ThinkingConfigParamUnion, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.IsZero() {
		return nil, nil
	}
	if config.Effort == lebro.ReasoningOff {
		disabled := claude.NewThinkingConfigDisabledParam()
		return &claude.ThinkingConfigParamUnion{OfDisabled: &disabled}, nil
	}
	if maxTokens <= 1024 {
		return nil, errors.New("lebro: Anthropic reasoning requires max tokens greater than 1024")
	}
	budget := config.BudgetTokens
	if budget == 0 {
		switch config.Effort {
		case lebro.ReasoningMinimal, lebro.ReasoningLow:
			budget = 1024
		case lebro.ReasoningMedium:
			budget = maxTokens / 2
		case lebro.ReasoningHigh:
			budget = (maxTokens * 3) / 4
		case lebro.ReasoningXHigh, lebro.ReasoningMax:
			budget = maxTokens - 1
		default:
			return nil, fmt.Errorf("lebro: Anthropic does not support reasoning effort %q", config.Effort)
		}
	}
	if budget < 1024 || budget >= maxTokens {
		return nil, fmt.Errorf("lebro: Anthropic reasoning budget %d must be at least 1024 and less than max tokens %d", budget, maxTokens)
	}
	return &claude.ThinkingConfigParamUnion{OfEnabled: &claude.ThinkingConfigEnabledParam{BudgetTokens: budget}}, nil
}

type anthropicReasoningDetail struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

func anthropicReasoningBlocks(reasoning lebro.ModelReasoning) ([]claude.ContentBlockParamUnion, error) {
	if reasoning.IsZero() || reasoning.Details == "" {
		return nil, nil
	}
	var details []anthropicReasoningDetail
	if err := json.Unmarshal(reasoning.Details.Raw(), &details); err != nil {
		return nil, fmt.Errorf("lebro: decode Anthropic reasoning details: %w", err)
	}
	blocks := make([]claude.ContentBlockParamUnion, 0, len(details))
	for _, detail := range details {
		switch detail.Type {
		case "thinking":
			// Compatible providers may stream thinking without a signature.
			// Such reasoning is display-only: without a signature the endpoint
			// cannot verify it, so it is not sent back.
			if detail.Signature == "" {
				continue
			}
			if detail.Thinking == "" {
				return nil, errors.New("lebro: Anthropic thinking details require signature and thinking text")
			}
			blocks = append(blocks, claude.NewThinkingBlock(detail.Signature, detail.Thinking))
		case "redacted_thinking":
			if detail.Data == "" {
				return nil, errors.New("lebro: Anthropic redacted thinking details require data")
			}
			blocks = append(blocks, claude.NewRedactedThinkingBlock(detail.Data))
		}
	}
	return blocks, nil
}

func newAnthropicReasoning(text string, details []anthropicReasoningDetail) lebro.ModelReasoning {
	reasoning := lebro.ModelReasoning{Text: text}
	if len(details) > 0 {
		encoded, _ := json.Marshal(details)
		reasoning.Details = lebro.NewModelReasoningDetails(encoded)
	}
	return reasoning
}

func reasoningText(details []anthropicReasoningDetail) string {
	var text strings.Builder
	for _, detail := range details {
		text.WriteString(detail.Thinking)
	}
	return text.String()
}

func (m *Model) response(request lebro.ModelRequest, result *claude.Message) (lebro.ModelResponse, error) {
	if result == nil {
		return lebro.ModelResponse{}, m.malformed(errors.New("lebro: empty Anthropic response"))
	}
	var text strings.Builder
	calls := make([]lebro.ModelToolCall, 0)
	details := make([]anthropicReasoningDetail, 0)
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			calls = append(calls, lebro.ModelToolCall{ID: block.ID, ToolID: lebro.ToolID(block.Name), Arguments: append(json.RawMessage(nil), block.Input...)})
		case "thinking":
			details = append(details, anthropicReasoningDetail{Type: block.Type, Thinking: block.Thinking, Signature: block.Signature})
		case "redacted_thinking":
			details = append(details, anthropicReasoningDetail{Type: block.Type, Data: block.Data})
		}
	}
	message := lebro.Message{Role: lebro.RoleAssistant, Content: text.String(), Reasoning: newAnthropicReasoning(reasoningText(details), details)}
	if len(calls) > 0 {
		encoded, err := lebro.NewModelToolCalls(calls...)
		if err != nil {
			return lebro.ModelResponse{}, m.malformed(err)
		}
		message.ToolCalls = encoded
	}
	if request.OutputSchema != nil && len(calls) == 0 {
		if !json.Valid([]byte(message.Content)) {
			return lebro.ModelResponse{}, m.malformed(errors.New("lebro: Anthropic structured output is not valid JSON"))
		}
		message.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(message.Content))
	}
	response := lebro.ModelResponse{Message: message, FinishReason: mapFinish(result.StopReason), Usage: mapUsage(result.Usage), Accounting: unavailableAccounting(m.pricingDomain, result.ID, result.Usage)}
	if result.ID != "" {
		response.Extension, _ = json.Marshal(map[string]string{"anthropic_id": result.ID, "anthropic_model": string(result.Model)})
	}
	if err := response.Validate(); err != nil {
		return lebro.ModelResponse{}, m.malformed(err)
	}
	return response, nil
}

func mapUsage(usage claude.Usage) lebro.ModelUsage {
	cacheWrite := usage.CacheCreationInputTokens
	return lebro.ModelUsage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		ReasoningTokens: usage.OutputTokensDetails.ThinkingTokens,
		CacheReadTokens: usage.CacheReadInputTokens, CacheWriteTokens: cacheWrite,
		CacheWrite1hTokens: usage.CacheCreation.Ephemeral1hInputTokens,
		TotalTokens:        usage.InputTokens + cacheWrite + usage.CacheReadInputTokens + usage.OutputTokens,
	}
}

func unavailableAccounting(domain lebro.PricingDomain, id string, usage claude.Usage) lebro.ModelAccounting {
	return lebro.ModelAccounting{
		ProviderRequestID: id, ServiceTier: string(usage.ServiceTier), Region: usage.InferenceGeo,
		Costs: []lebro.ModelCost{lebro.UnavailableModelCost(domain, lebro.CostUnavailableProviderOmitted)},
	}
}

func mapFinish(reason claude.StopReason) lebro.FinishReason {
	switch reason {
	case "tool_use":
		return lebro.FinishReasonToolCalls
	case "max_tokens", "model_context_window_exceeded":
		return lebro.FinishReasonLength
	case "refusal":
		return lebro.FinishReasonContent
	default:
		return lebro.FinishReasonStop
	}
}
func (m *Model) invalid(err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorInvalidRequest, Provider: string(m.ProviderID()), Message: err.Error(), Err: err}
}
func (m *Model) malformed(err error) error {
	return &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: string(m.ProviderID()), Message: err.Error(), Err: err}
}
func (m *Model) error(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	var apiErr *claude.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		return &lebro.ModelError{Kind: statusToKind(status), Provider: string(m.ProviderID()), StatusCode: status, Message: err.Error(), Err: err}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		kind := lebro.ModelErrorTransport
		if networkErr.Timeout() {
			kind = lebro.ModelErrorTimeout
		}
		return &lebro.ModelError{Kind: kind, Provider: string(m.ProviderID()), Message: err.Error(), Err: err}
	}
	return &lebro.ModelError{Kind: lebro.ModelErrorUnavailable, Provider: string(m.ProviderID()), Message: err.Error(), Err: err}
}

func statusToKind(status int) lebro.ModelErrorKind {
	switch {
	case status == http.StatusUnauthorized:
		return lebro.ModelErrorAuthentication
	case status == http.StatusForbidden:
		return lebro.ModelErrorPermissionDenied
	case status == http.StatusNotFound:
		return lebro.ModelErrorNotFound
	case status == http.StatusTooManyRequests:
		return lebro.ModelErrorRateLimited
	case status >= 400 && status < 500:
		return lebro.ModelErrorInvalidRequest
	case status >= 500 && status < 600:
		return lebro.ModelErrorUnavailable
	default:
		return lebro.ModelErrorUnknown
	}
}

type anthropicStream struct {
	stream interface {
		Next() bool
		Current() claude.MessageStreamEventUnion
		Err() error
		Close() error
	}
	values  chan lebro.StreamDelta
	done    chan struct{}
	errorFn func(context.Context, error) error
	once    sync.Once
	// provider labels stream-level errors; pricingDomain labels accounting.
	provider      string
	pricingDomain lebro.PricingDomain
}

func (r *anthropicStream) send(delta lebro.StreamDelta) bool {
	select {
	case r.values <- delta:
		return true
	case <-r.done:
		return false
	}
}

func (r *anthropicStream) start(ctx context.Context, request lebro.ModelRequest) {
	go func() {
		defer close(r.values)
		tools := map[int64]*lebro.ModelToolCall{}
		thinking := map[int64]*strings.Builder{}
		thinkingSignatures := map[int64]string{}
		redacted := map[int64]string{}
		var text strings.Builder
		var finish = lebro.FinishReasonStop
		var usage lebro.ModelUsage
		var accounting lebro.ModelAccounting
		for r.stream.Next() {
			event := r.stream.Current()
			switch event.Type {
			case "message_start":
				usage = mapUsage(event.Message.Usage)
				accounting = unavailableAccounting(r.pricingDomain, event.Message.ID, event.Message.Usage)
			case "content_block_start":
				if event.ContentBlock.Type == "tool_use" {
					tools[event.Index] = &lebro.ModelToolCall{ID: event.ContentBlock.ID, ToolID: lebro.ToolID(event.ContentBlock.Name)}
				}
				if event.ContentBlock.Type == "thinking" {
					thinking[event.Index] = &strings.Builder{}
				}
				if event.ContentBlock.Type == "redacted_thinking" {
					redacted[event.Index] = event.ContentBlock.Data
				}
			case "content_block_delta":
				if event.Delta.Text != "" && event.Delta.Thinking != "" {
					r.send(lebro.StreamDelta{Err: &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: r.provider, Message: "lebro: Anthropic stream delta contains text and thinking in one content block"}})
					return
				}
				if event.Delta.Text != "" {
					text.WriteString(event.Delta.Text)
					if !r.send(lebro.StreamDelta{
						Parts: []lebro.StreamContentPart{{Kind: lebro.StreamContentPartText, Text: event.Delta.Text}},
						Text:  event.Delta.Text,
					}) {
						return
					}
				}
				if call := tools[event.Index]; call != nil {
					call.Arguments = append(call.Arguments, event.Delta.PartialJSON...)
				}
				if event.Delta.Thinking != "" {
					if block := thinking[event.Index]; block != nil {
						block.WriteString(event.Delta.Thinking)
					}
					reasoning := lebro.ModelReasoning{Text: event.Delta.Thinking}
					if !r.send(lebro.StreamDelta{
						Parts:     []lebro.StreamContentPart{{Kind: lebro.StreamContentPartReasoning, Text: reasoning.Text}},
						Reasoning: reasoning,
					}) {
						return
					}
				}
				if event.Delta.Signature != "" {
					thinkingSignatures[event.Index] += event.Delta.Signature
				}
			case "content_block_stop":
				if call := tools[event.Index]; call != nil {
					if len(call.Arguments) == 0 {
						call.Arguments = json.RawMessage(`{}`)
					}
					if !json.Valid(call.Arguments) {
						r.send(lebro.StreamDelta{Err: &lebro.ModelError{Kind: lebro.ModelErrorMalformedResponse, Provider: r.provider, Message: "lebro: Anthropic streamed tool arguments are not valid JSON"}})
						return
					}
					copy := *call
					if !r.send(lebro.StreamDelta{ToolCall: &copy}) {
						return
					}
					delete(tools, event.Index)
				}
				// An unsigned block from a compatible provider is kept as
				// display-only detail; replay later skips it.
				if block := thinking[event.Index]; block != nil && (block.Len() > 0 || thinkingSignatures[event.Index] != "") {
					detail := anthropicReasoningDetail{Type: "thinking", Thinking: block.String(), Signature: thinkingSignatures[event.Index]}
					reasoning := newAnthropicReasoning("", []anthropicReasoningDetail{detail})
					if !r.send(lebro.StreamDelta{
						Parts:     []lebro.StreamContentPart{{Kind: lebro.StreamContentPartReasoning, ReasoningDetails: reasoning.Details}},
						Reasoning: reasoning,
					}) {
						return
					}
				}
				delete(thinking, event.Index)
				delete(thinkingSignatures, event.Index)
				if data, ok := redacted[event.Index]; ok {
					reasoning := newAnthropicReasoning("", []anthropicReasoningDetail{{Type: "redacted_thinking", Data: data}})
					if !r.send(lebro.StreamDelta{
						Parts:     []lebro.StreamContentPart{{Kind: lebro.StreamContentPartReasoning, ReasoningDetails: reasoning.Details}},
						Reasoning: reasoning,
					}) {
						return
					}
					delete(redacted, event.Index)
				}
			case "message_delta":
				finish = mapFinish(event.Delta.StopReason)
				usage.OutputTokens = event.Usage.OutputTokens
				usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
				usage.TotalTokens = usage.InputTokens + usage.CacheWriteTokens + usage.CacheReadTokens + usage.OutputTokens
			}
		}
		if err := r.stream.Err(); err != nil {
			r.send(lebro.StreamDelta{Err: r.errorFn(ctx, err)})
			return
		}
		terminal := lebro.StreamDelta{FinishReason: finish, Usage: usage, Accounting: accounting.Clone()}
		if request.OutputSchema != nil && json.Valid([]byte(text.String())) {
			terminal.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(text.String()))
		}
		r.send(terminal)
	}()
}
func (r *anthropicStream) Next() (lebro.StreamDelta, error) {
	delta, ok := <-r.values
	if !ok {
		return lebro.StreamDelta{}, io.EOF
	}
	return delta, nil
}
func (r *anthropicStream) Close() error {
	var err error
	r.once.Do(func() {
		close(r.done)
		err = r.stream.Close()
	})
	return err
}
