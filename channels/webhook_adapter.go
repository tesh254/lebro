package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// maxWebhookBodyBytes bounds a webhook body so a single client cannot make the
// server allocate without limit. One megabyte is far above a realistic message
// payload and far below a memory problem.
const maxWebhookBodyBytes = 1 << 20

// WebhookAdapterConfig configures a generic HMAC webhook adapter. It is
// provider-neutral: any platform that can POST a JSON body and sign it with a
// shared secret over the raw payload works without a platform-specific
// dependency.
type WebhookAdapterConfig struct {
	// Platform is the adapter's platform identifier, appearing in the webhook
	// route and every decoded ConversationRef. Required and must be URL-safe.
	Platform string
	// Secret is the shared HMAC-SHA256 key. The webhook signs the raw request
	// body with it and sends the hex digest in the SignatureHeader. Required.
	Secret []byte
	// SignatureHeader is the request header carrying the hex-encoded HMAC over
	// the raw body. When empty, "X-Signature" is used.
	SignatureHeader string
	// TimestampHeader is the request header carrying a Unix-seconds timestamp
	// that is bound into the signature to prevent replay. When empty,
	// "X-Timestamp" is used.
	TimestampHeader string
	// MaxSkew rejects a request whose timestamp differs from now by more than
	// this. When zero, DefaultWebhookMaxSkew is used. A negative value disables
	// the timestamp check, which also removes replay protection.
	MaxSkew time.Duration
	// ReplyURL is the endpoint the adapter POSTs replies to. Each reply chunk
	// is delivered as a JSON body. When empty, Send is a no-op, which suits a
	// platform whose inbound and reply channels are separate and wired
	// elsewhere.
	ReplyURL string
	// Client is the HTTP client used to deliver replies. When nil,
	// http.DefaultClient is used.
	Client *http.Client
	// Now returns the current time for skew checks. When nil, time.Now is used.
	// Inject a fixed clock for deterministic tests.
	Now func() time.Time
}

// DefaultWebhookMaxSkew is the timestamp tolerance used when a
// WebhookAdapterConfig leaves MaxSkew zero.
const DefaultWebhookMaxSkew = 5 * time.Minute

// webhookInbound is the JSON shape the generic adapter expects. It mirrors the
// neutral InboundMessage so a platform integrator maps their payload to a small
// stable contract.
type webhookInbound struct {
	MessageID      string            `json:"message_id"`
	ConversationID string            `json:"conversation_id"`
	UserID         string            `json:"user_id"`
	UserName       string            `json:"user_name"`
	Tenant         string            `json:"tenant"`
	Text           string            `json:"text"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// webhookOutbound is the JSON shape delivered to ReplyURL for each reply chunk.
type webhookOutbound struct {
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
	Final          bool   `json:"final"`
	Format         string `json:"format"`
}

// WebhookAdapter is a provider-neutral HMAC-authenticated webhook channel. It
// verifies each request by recomputing an HMAC-SHA256 over the raw body bound
// to a timestamp, decodes a small neutral JSON payload, and delivers replies by
// POSTing JSON to a configured URL.
type WebhookAdapter struct {
	platform  string
	secret    []byte
	sigHeader string
	tsHeader  string
	maxSkew   time.Duration
	replyURL  string
	client    *http.Client
	now       func() time.Time
}

// NewWebhookAdapter constructs a WebhookAdapter. It returns an error when the
// platform or secret is missing, because neither has a safe default: an empty
// platform yields an ambiguous route and an empty secret disables
// authentication.
func NewWebhookAdapter(config WebhookAdapterConfig) (*WebhookAdapter, error) {
	if config.Platform == "" {
		return nil, errors.New("lebro/channels: webhook adapter requires a platform")
	}
	if len(config.Secret) == 0 {
		return nil, errors.New("lebro/channels: webhook adapter requires a secret")
	}
	sigHeader := config.SignatureHeader
	if sigHeader == "" {
		sigHeader = "X-Signature"
	}
	tsHeader := config.TimestampHeader
	if tsHeader == "" {
		tsHeader = "X-Timestamp"
	}
	maxSkew := config.MaxSkew
	if maxSkew == 0 {
		maxSkew = DefaultWebhookMaxSkew
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	// Copy the secret so a later mutation of the caller's slice cannot silently
	// change the key this adapter verifies against.
	secret := append([]byte(nil), config.Secret...)
	return &WebhookAdapter{
		platform:  config.Platform,
		secret:    secret,
		sigHeader: sigHeader,
		tsHeader:  tsHeader,
		maxSkew:   maxSkew,
		replyURL:  config.ReplyURL,
		client:    client,
		now:       now,
	}, nil
}

// Platform reports the adapter's platform identifier.
func (a *WebhookAdapter) Platform() string { return a.platform }

// Verify authenticates the request by recomputing the HMAC over the timestamp
// and raw body and comparing it in constant time to the supplied signature. The
// timestamp is bound into the signed value so a captured body cannot be
// replayed under a fresh timestamp, and the timestamp itself is bounded by
// MaxSkew unless the check is disabled. It returns the buffered body so Decode
// reads it without a second read.
func (a *WebhookAdapter) Verify(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxWebhookBodyBytes {
		return nil, errors.New("lebro/channels: webhook body too large")
	}

	timestamp := r.Header.Get(a.tsHeader)
	if a.maxSkew >= 0 {
		if timestamp == "" {
			return nil, errors.New("lebro/channels: missing timestamp header")
		}
		seconds, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("lebro/channels: invalid timestamp: %w", err)
		}
		skew := a.now().Sub(time.Unix(seconds, 0))
		if skew < 0 {
			skew = -skew
		}
		if skew > a.maxSkew {
			return nil, errors.New("lebro/channels: timestamp outside allowed skew")
		}
	}

	provided := r.Header.Get(a.sigHeader)
	if provided == "" {
		return nil, errors.New("lebro/channels: missing signature header")
	}
	providedMAC, err := hex.DecodeString(provided)
	if err != nil {
		return nil, fmt.Errorf("lebro/channels: invalid signature encoding: %w", err)
	}

	mac := hmac.New(sha256.New, a.secret)
	// The timestamp is prefixed with a separator so it cannot be shifted into
	// the body's leading bytes to forge a different (timestamp, body) pair that
	// hashes the same.
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	expected := mac.Sum(nil)

	if !hmac.Equal(providedMAC, expected) {
		return nil, errors.New("lebro/channels: signature mismatch")
	}
	return body, nil
}

// Decode parses the verified body into a neutral InboundMessage. A body with an
// empty conversation ID and empty text is treated as a non-message (for
// example, a platform ping) and returns ok false so the handler acknowledges it
// without running the agent.
func (a *WebhookAdapter) Decode(_ *http.Request, body []byte) (InboundMessage, bool, error) {
	if len(body) == 0 {
		return InboundMessage{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload webhookInbound
	if err := decoder.Decode(&payload); err != nil {
		return InboundMessage{}, false, err
	}
	if payload.ConversationID == "" && payload.Text == "" {
		return InboundMessage{}, false, nil
	}
	return InboundMessage{
		Conversation:      ConversationRef{Platform: a.platform, ID: payload.ConversationID},
		ProviderMessageID: payload.MessageID,
		Sender: ChannelIdentity{
			ProviderUserID: payload.UserID,
			DisplayName:    payload.UserName,
			Tenant:         payload.Tenant,
		},
		Text:     payload.Text,
		Metadata: payload.Metadata,
	}, true, nil
}

// Send delivers one reply chunk by POSTing JSON to the configured ReplyURL. It
// is a no-op when no ReplyURL is configured. A non-2xx response is an error so
// the handler can surface a delivery failure.
func (a *WebhookAdapter) Send(ctx context.Context, conversation ConversationRef, message OutboundMessage) error {
	if a.replyURL == "" {
		return nil
	}
	format := string(message.Format)
	if message.Format == FormatMarkdown {
		format = "markdown"
	}
	payload, err := json.Marshal(webhookOutbound{
		ConversationID: conversation.ID,
		Text:           message.Text,
		Final:          message.Final,
		Format:         format,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.replyURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be reused rather than closed.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lebro/channels: reply delivery failed with status %d", resp.StatusCode)
	}
	return nil
}

var _ Adapter = (*WebhookAdapter)(nil)
