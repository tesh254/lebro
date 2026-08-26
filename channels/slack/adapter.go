package slack

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
	"strings"
	"time"

	"github.com/tesh254/lebro/channels"
)

const (
	platform              = "slack"
	defaultMaxClockSkew   = 5 * time.Minute
	defaultMaxBodyBytes   = 1 << 20
	defaultPostMessageURL = "https://slack.com/api/chat.postMessage"
	slackSignatureVersion = "v0"
	slackRequestTimestamp = "X-Slack-Request-Timestamp"
	slackSignature        = "X-Slack-Signature"
)

// Config configures a Slack Events API adapter.
type Config struct {
	// SigningSecret is the Slack app signing secret used to verify inbound
	// requests. It is required.
	SigningSecret string
	// BotToken authorizes outbound chat.postMessage calls. It is required.
	BotToken string
	// HTTPClient performs outbound requests. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// PostMessageURL overrides Slack's chat.postMessage endpoint. It is useful
	// for Slack-compatible deployments and tests.
	PostMessageURL string
	// MaxClockSkew limits accepted request timestamp age. Zero uses five
	// minutes, Slack's documented replay window.
	MaxClockSkew time.Duration
	// MaxBodyBytes limits the signed request body read. Zero uses 1 MiB.
	MaxBodyBytes int64
	// Now supplies the current time. Nil uses time.Now and exists to make
	// timestamp checks deterministic in tests.
	Now func() time.Time
}

// Adapter is a concurrent-safe Slack Events API channel adapter.
type Adapter struct {
	signingSecret []byte
	botToken      string
	httpClient    *http.Client
	postMessage   string
	maxClockSkew  time.Duration
	maxBodyBytes  int64
	now           func() time.Time
}

// New constructs a Slack adapter. Both request verification and reply delivery
// are configured together so an exposed adapter cannot accept events that it is
// unable to answer.
func New(config Config) (*Adapter, error) {
	if config.SigningSecret == "" {
		return nil, errors.New("lebro/channels/slack: SigningSecret is required")
	}
	if config.BotToken == "" {
		return nil, errors.New("lebro/channels/slack: BotToken is required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.PostMessageURL == "" {
		config.PostMessageURL = defaultPostMessageURL
	}
	if config.MaxClockSkew == 0 {
		config.MaxClockSkew = defaultMaxClockSkew
	}
	if config.MaxClockSkew < 0 {
		return nil, errors.New("lebro/channels/slack: MaxClockSkew must not be negative")
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxBodyBytes < 1 {
		return nil, errors.New("lebro/channels/slack: MaxBodyBytes must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Adapter{
		signingSecret: []byte(config.SigningSecret),
		botToken:      config.BotToken,
		httpClient:    config.HTTPClient,
		postMessage:   config.PostMessageURL,
		maxClockSkew:  config.MaxClockSkew,
		maxBodyBytes:  config.MaxBodyBytes,
		now:           config.Now,
	}, nil
}

// Platform reports Slack's stable channels platform identifier.
func (*Adapter) Platform() string { return platform }

// Verify validates Slack's v0 HMAC-SHA256 signature over the exact raw body.
func (a *Adapter) Verify(r *http.Request) ([]byte, error) {
	if r == nil {
		return nil, errors.New("lebro/channels/slack: request is nil")
	}
	timestamp := r.Header.Get(slackRequestTimestamp)
	if timestamp == "" {
		return nil, errors.New("lebro/channels/slack: missing request timestamp")
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || seconds < 0 {
		return nil, errors.New("lebro/channels/slack: invalid request timestamp")
	}
	requestTime := time.Unix(seconds, 0)
	if delta := a.now().Sub(requestTime); delta > a.maxClockSkew || delta < -a.maxClockSkew {
		return nil, errors.New("lebro/channels/slack: request timestamp outside allowed skew")
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, a.maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("lebro/channels/slack: read request body: %w", err)
	}
	if int64(len(body)) > a.maxBodyBytes {
		return nil, errors.New("lebro/channels/slack: request body exceeds MaxBodyBytes")
	}
	signature := r.Header.Get(slackSignature)
	if !validSignature(signature) {
		return nil, errors.New("lebro/channels/slack: invalid request signature")
	}
	mac := hmac.New(sha256.New, a.signingSecret)
	_, _ = mac.Write([]byte(slackSignatureVersion + ":" + timestamp + ":"))
	_, _ = mac.Write(body)
	want := slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return nil, errors.New("lebro/channels/slack: request signature mismatch")
	}
	return body, nil
}

func validSignature(signature string) bool {
	if !strings.HasPrefix(signature, slackSignatureVersion+"=") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(signature, slackSignatureVersion+"="))
	return err == nil
}

// WebhookResponse returns Slack's signed URL verification challenge verbatim.
func (*Adapter) WebhookResponse(_ *http.Request, body []byte) ([]byte, bool, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("lebro/channels/slack: decode webhook envelope: %w", err)
	}
	if envelope.Type != "url_verification" {
		return nil, false, nil
	}
	if envelope.Challenge == "" {
		return nil, false, errors.New("lebro/channels/slack: url verification challenge is empty")
	}
	return []byte(envelope.Challenge), true, nil
}

// WebhookAcknowledgement promptly acknowledges accepted Events API deliveries.
func (*Adapter) WebhookAcknowledgement(_ *http.Request, _ []byte) ([]byte, string, error) {
	return nil, "", nil
}

// Decode converts a verified Slack message event into the neutral channel
// contract. Bot and subtype messages are ignored to prevent reply loops and
// avoid treating message mutations as new user turns.
func (*Adapter) Decode(_ *http.Request, body []byte) (channels.InboundMessage, bool, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return channels.InboundMessage{}, false, fmt.Errorf("lebro/channels/slack: decode webhook envelope: %w", err)
	}
	if envelope.Type != "event_callback" {
		return channels.InboundMessage{}, false, nil
	}
	event := envelope.Event
	if event.Type != "message" || event.BotID != "" || event.Subtype != "" {
		return channels.InboundMessage{}, false, nil
	}
	if envelope.EventID == "" || envelope.TeamID == "" || event.Channel == "" || event.TS == "" || event.User == "" {
		return channels.InboundMessage{}, false, errors.New("lebro/channels/slack: message event is missing required identifiers")
	}
	threadTS := event.ThreadTS
	if threadTS == "" {
		threadTS = event.TS
	}
	conversation := conversationID(envelope.TeamID, event.Channel, threadTS)
	return channels.InboundMessage{
		Conversation:      channels.ConversationRef{Platform: platform, ID: conversation},
		ProviderMessageID: envelope.EventID,
		Sender:            channels.ChannelIdentity{ProviderUserID: event.User, Tenant: envelope.TeamID},
		Text:              event.Text,
		Metadata: map[string]string{
			"slack.team_id":    envelope.TeamID,
			"slack.channel_id": event.Channel,
			"slack.message_ts": event.TS,
			"slack.thread_ts":  threadTS,
		},
	}, true, nil
}

// Send posts only the terminal reply. Slack's chat.postMessage rate limits make
// one completed thread reply preferable to one API request per model delta.
func (a *Adapter) Send(ctx context.Context, conversation channels.ConversationRef, message channels.OutboundMessage) error {
	if !message.Final {
		return nil
	}
	if conversation.Platform != platform {
		return fmt.Errorf("lebro/channels/slack: cannot send to platform %q", conversation.Platform)
	}
	_, channel, threadTS, ok := parseConversationID(conversation.ID)
	if !ok {
		return errors.New("lebro/channels/slack: invalid conversation ID")
	}
	payload := postMessageRequest{Channel: channel, Text: message.Text, ThreadTS: threadTS, Mrkdwn: message.Format != channels.FormatPlain}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("lebro/channels/slack: encode chat.postMessage request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.postMessage, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("lebro/channels/slack: create chat.postMessage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lebro/channels/slack: call chat.postMessage: %w", err)
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if err != nil {
		return fmt.Errorf("lebro/channels/slack: read chat.postMessage response: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("lebro/channels/slack: close chat.postMessage response: %w", closeErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("lebro/channels/slack: chat.postMessage returned HTTP %d", response.StatusCode)
	}
	var result postMessageResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return fmt.Errorf("lebro/channels/slack: decode chat.postMessage response: %w", err)
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "unknown error"
		}
		return fmt.Errorf("lebro/channels/slack: chat.postMessage: %s", result.Error)
	}
	return nil
}

type eventEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	TeamID    string `json:"team_id"`
	EventID   string `json:"event_id"`
	Event     struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		Text     string `json:"text"`
		Channel  string `json:"channel"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		BotID    string `json:"bot_id"`
		Subtype  string `json:"subtype"`
	} `json:"event"`
}

type postMessageRequest struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	ThreadTS string `json:"thread_ts"`
	Mrkdwn   bool   `json:"mrkdwn"`
}

type postMessageResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func conversationID(teamID, channelID, threadTS string) string {
	return teamID + "\x00" + channelID + "\x00" + threadTS
}

func parseConversationID(value string) (teamID, channelID, threadTS string, ok bool) {
	parts := strings.Split(value, "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

var (
	_ channels.Adapter             = (*Adapter)(nil)
	_ channels.WebhookResponder    = (*Adapter)(nil)
	_ channels.WebhookAcknowledger = (*Adapter)(nil)
)
