// Package mediahttp implements the shared OpenAI-style media wire protocols.
// Provider entry points select the dialect explicitly; URL guessing is avoided.
package mediahttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tesh254/lebro"
)

type Config struct {
	Policy     lebro.Policy
	Attempts   lebro.ModelAttemptRepository
	APIKey     string
	BaseURL    string
	Model      string
	HTTPClient *http.Client
	// Timeout bounds request lifetime; zero defaults to five minutes. Live
	// sessions use caller deadlines and a per-message idle timeout instead.
	Timeout time.Duration
	// Capabilities explicitly describes an otherwise unknown configured model.
	// Only fields implemented by this adapter may be enabled.
	Capabilities *lebro.MediaCapabilities
}
type Client struct {
	policy                     lebro.Policy
	attempts                   lebro.ModelAttemptRepository
	key, base, model, provider string
	http                       *http.Client
	timeout                    time.Duration
	caps                       lebro.MediaCapabilities
}

func New(c Config, provider string) (*Client, error) {
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.Model) == "" {
		return nil, invalid("API key and model are required")
	}
	if c.Timeout < 0 {
		return nil, invalid("negative timeout")
	}
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Minute
	}
	if c.BaseURL == "" {
		if provider == "openrouter" {
			c.BaseURL = "https://openrouter.ai/api/v1"
		} else {
			c.BaseURL = "https://api.openai.com/v1"
		}
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalid("invalid media base URL")
	}
	caps := Profile(provider, c.Model)
	if c.Capabilities != nil {
		caps = *c.Capabilities
	}
	caps.Provider = provider
	caps.Model = c.Model
	if caps.MaxInputBytes == 0 {
		caps.MaxInputBytes = 25 << 20
	}
	if caps.MaxOutputBytes == 0 {
		caps.MaxOutputBytes = 64 << 20
	}
	if caps.MaxTextBytes == 0 {
		caps.MaxTextBytes = 4096
	}
	if caps.MaxCount == 0 {
		caps.MaxCount = 1
	}
	if caps.MaxInputBytes < 1 || caps.MaxOutputBytes < 1 || caps.MaxOutputBytes > 1<<30 || caps.MaxInputBytes > 1<<30 || caps.MaxTextBytes < 1 || caps.MaxCount < 1 || caps.MaxCount > 10 {
		return nil, invalid("invalid media limits")
	}
	if caps.RemoteCancel || (provider == "openrouter" && caps.StreamingTranscription) || (provider == "openai" && caps.Video) {
		return nil, unsupported("configured mode is not implemented by this adapter")
	}
	if provider == "openai" && (len(caps.Resolutions) > 0 || len(caps.AspectRatios) > 0 || caps.StreamingTranscription && c.Model != "gpt-live-transcribe") {
		return nil, unsupported("configured options require a different wire protocol")
	}
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	// Neither signed content redirects nor configurable hosts may forward keys.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	b, _ := json.Marshal(caps)
	_ = json.Unmarshal(b, &caps)
	return &Client{policy: c.Policy, attempts: c.Attempts, key: c.APIKey, base: strings.TrimRight(c.BaseURL, "/"), model: c.Model, provider: provider, http: &client, timeout: c.Timeout, caps: caps}, nil
}
func (c *Client) Capabilities() lebro.MediaCapabilities {
	b, _ := json.Marshal(c.caps)
	var out lebro.MediaCapabilities
	_ = json.Unmarshal(b, &out)
	return out
}
func Profile(provider, model string) lebro.MediaCapabilities {
	p := lebro.MediaCapabilities{MaxCount: 1, MaxTextBytes: 4096}
	base := strings.TrimPrefix(model, "openai/")
	if slices.Contains([]string{"gpt-image-1", "gpt-image-1.5"}, base) {
		p.Image = true
		p.MaxCount = 10
		p.MaxTextBytes = 32000
		p.OutputFormats = []string{"png", "jpeg", "webp"}
		p.Sizes = []string{"1024x1024", "1536x1024", "1024x1536", "auto"}
		p.Qualities = []string{"auto", "low", "medium", "high"}
	}
	if slices.Contains([]string{"whisper-1", "whisper-large-v3", "gpt-4o-transcribe", "gpt-4o-mini-transcribe"}, base) {
		p.Transcription = true
		p.Timestamps = strings.HasPrefix(base, "whisper")
		p.Language = true
		p.PromptHint = provider == "openai"
		p.InputMIMETypes = []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/flac"}
	}
	if slices.Contains([]string{"tts-1", "tts-1-hd", "gpt-4o-mini-tts", "gpt-4o-mini-tts-2025-12-15"}, base) {
		p.Speech = true
		p.StreamingSpeech = true
		p.Speed = true
		p.OutputFormats = []string{"mp3", "pcm"}
		p.Voices = []string{"alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer", "verse", "marin", "cedar"}
		if base == "tts-1" || base == "tts-1-hd" {
			p.Voices = []string{"alloy", "echo", "fable", "onyx", "nova", "shimmer"}
		}
	}
	if provider == "openai" && model == "gpt-live-transcribe" {
		p.StreamingTranscription = true
		p.Language = true
		p.PromptHint = true
		p.InputMIMETypes = []string{"audio/pcm"}
	}
	if provider == "openrouter" && model == "google/veo-3.1" {
		p.Video = true
		p.MaxTextBytes = 32000
		p.Durations = []int{4, 6, 8}
		p.Resolutions = []string{"720p", "1080p"}
		p.AspectRatios = []string{"16:9", "9:16"}
		p.OutputFormats = []string{"mp4"}
	}
	return p
}
func invalid(s string) error { return &lebro.MediaError{Kind: lebro.MediaErrorValidation, Message: s} }
func unsupported(s string) error {
	return &lebro.MediaError{Kind: lebro.MediaErrorUnsupported, Message: s}
}
func malformed(s string) error { return &lebro.MediaError{Kind: lebro.MediaErrorMalformed, Message: s} }
func allowed(value string, values []string) error {
	if value != "" && !slices.Contains(values, value) {
		return unsupported("requested option is not supported")
	}
	return nil
}
func (c *Client) text(o lebro.MediaOperation, s string) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" || len(s) > c.caps.MaxTextBytes {
		return invalid("empty or over-limit text")
	}
	return nil
}
func (c *Client) info(o lebro.MediaOperation, h http.Header) lebro.MediaResultInfo {
	id := h.Get("X-Request-Id")
	if id == "" {
		id = h.Get("X-Generation-Id")
	}
	if !safeID(id) {
		id = ""
	}
	return lebro.MediaResultInfo{Operation: o, Provider: c.provider, Model: c.model, ProviderRequestID: id}
}
func safeID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.ContainsAny(id, "/?#\r\n\x00")
}
func (c *Client) request(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, invalid("invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		kind := lebro.MediaErrorTransport
		if ctx.Err() != nil {
			err = ctx.Err()
			if errors.Is(err, context.Canceled) {
				kind = lebro.MediaErrorCancelled
			} else {
				kind = lebro.MediaErrorTimeout
			}
		}
		return nil, &lebro.MediaError{Kind: kind, Message: "media request failed", Cause: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	return nil, responseError(resp)
}
func responseError(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	kind := lebro.MediaErrorRemote
	retry := false
	switch resp.StatusCode {
	case 400, 413, 422:
		kind = lebro.MediaErrorValidation
	case 401:
		kind = lebro.MediaErrorAuthentication
	case 403:
		kind = lebro.MediaErrorAuthorization
	case 404, 410:
		kind = lebro.MediaErrorExpired
	case 429:
		kind = lebro.MediaErrorRateLimit
		retry = true
	case 408, 504:
		kind = lebro.MediaErrorTimeout
		retry = true
	case 500, 502, 503:
		retry = true
	}
	var wire struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&wire)
	var code string
	_ = json.Unmarshal(wire.Error.Code, &code)
	switch code {
	case "content_policy_violation", "moderation_blocked", "safety_violation":
		kind = lebro.MediaErrorRefusal
	case "insufficient_quota":
		kind = lebro.MediaErrorQuota
		retry = false
	default:
		code = ""
	}
	hint := time.Duration(0)
	if n, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); e == nil && n > 0 {
		hint = time.Duration(n) * time.Second
	} else if t, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil && t.After(time.Now()) {
		hint = time.Until(t)
	}
	return &lebro.MediaError{Kind: kind, Message: "provider rejected media request", ProviderCode: code, Retryable: retry, RetryAfter: hint}
}
func (c *Client) json(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, invalid("invalid request JSON")
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, path, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readBounded(resp.Body, c.caps.MaxOutputBytes)
	if err != nil {
		return resp.Header, err
	}
	if err = json.Unmarshal(b, out); err != nil {
		return resp.Header, malformed("invalid provider JSON")
	}
	return resp.Header, nil
}
func readBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if int64(len(b)) > max {
		return nil, invalid("media exceeds byte limit")
	}
	return b, err
}
func usage(raw map[string]json.RawMessage) lebro.MediaUsage {
	u := lebro.MediaUsage{Units: map[string]json.Number{}}
	for k, b := range raw {
		if k == "cost" {
			var n json.Number
			if json.Unmarshal(b, &n) == nil {
				if v, e := n.Float64(); e == nil && v >= 0 {
					u.CostUSD = &n
				}
			}
			continue
		}
		var n json.Number
		if json.Unmarshal(b, &n) == nil {
			if v, e := n.Float64(); e == nil && v >= 0 {
				u.Units[k] = n
			}
		}
	}
	return u
}

func (c *Client) authorize(ctx context.Context, o lebro.MediaOperation, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if scope, ok := lebro.RuntimeScopeFromContext(ctx); ok && scope != o.Scope {
		return &lebro.MediaError{Kind: lebro.MediaErrorAuthorization, Message: "media scope mismatch"}
	}
	if c.policy != nil {
		identity, _ := lebro.IdentityFromContext(ctx)
		decision := c.policy.Authorize(ctx, identity, lebro.Action(action), lebro.Resource{Kind: "media", ID: o.ID, Tenant: o.Scope.Namespace, OwnerID: o.Scope.OwnerID})
		if !decision.Allowed {
			return &lebro.MediaError{Kind: lebro.MediaErrorAuthorization, Message: "media operation denied", Cause: lebro.ErrPolicyDenied}
		}
	}
	return nil
}
func (c *Client) record(ctx context.Context, info lebro.MediaResultInfo, o lebro.MediaOperation, kind string, start time.Time, outcome error) error {
	if info.Operation.ID == "" {
		info = c.info(o, nil)
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return lebro.RecordMediaAttempt(saveCtx, c.attempts, info, kind, start, time.Now().UTC(), outcome)
}
