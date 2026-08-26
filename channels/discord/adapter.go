package discord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tesh254/lebro/channels"
)

const (
	platform          = "discord"
	defaultAPIBaseURL = "https://discord.com/api/v10"
	defaultMaxBody    = 1 << 20
	maxMessageRunes   = 2000
)

// Config configures a Discord application-command interaction adapter.
type Config struct {
	// PublicKey is the application public key from the Discord developer portal,
	// encoded as 64 hexadecimal characters. It is required for inbound request
	// verification.
	PublicKey string
	// HTTPClient performs Discord webhook requests. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// APIBaseURL overrides Discord's API v10 base URL for compatible deployments
	// and tests.
	APIBaseURL string
	// MaxBodyBytes limits inbound request reads. Zero uses 1 MiB.
	MaxBodyBytes int64
}

// Adapter is a concurrent-safe Discord interaction adapter.
type Adapter struct {
	publicKey    ed25519.PublicKey
	httpClient   *http.Client
	apiBaseURL   string
	maxBodyBytes int64
}

// New constructs a Discord interaction adapter.
func New(config Config) (*Adapter, error) {
	publicKey, err := hex.DecodeString(config.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("lebro/channels/discord: PublicKey must be a 32-byte hexadecimal Ed25519 key")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = defaultAPIBaseURL
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBody
	}
	if config.MaxBodyBytes < 1 {
		return nil, errors.New("lebro/channels/discord: MaxBodyBytes must be positive")
	}
	return &Adapter{publicKey: ed25519.PublicKey(publicKey), httpClient: config.HTTPClient, apiBaseURL: strings.TrimRight(config.APIBaseURL, "/"), maxBodyBytes: config.MaxBodyBytes}, nil
}

// Platform reports Discord's stable channels platform identifier.
func (*Adapter) Platform() string { return platform }

// Verify checks Discord's Ed25519 signature over timestamp concatenated with
// the exact raw request body.
func (a *Adapter) Verify(r *http.Request) ([]byte, error) {
	if r == nil {
		return nil, errors.New("lebro/channels/discord: request is nil")
	}
	timestamp := r.Header.Get("X-Signature-Timestamp")
	signature, err := hex.DecodeString(r.Header.Get("X-Signature-Ed25519"))
	if timestamp == "" || err != nil || len(signature) != ed25519.SignatureSize {
		return nil, errors.New("lebro/channels/discord: invalid request signature headers")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, a.maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("lebro/channels/discord: read request body: %w", err)
	}
	if int64(len(body)) > a.maxBodyBytes {
		return nil, errors.New("lebro/channels/discord: request body exceeds MaxBodyBytes")
	}
	message := make([]byte, 0, len(timestamp)+len(body))
	message = append(message, timestamp...)
	message = append(message, body...)
	if !ed25519.Verify(a.publicKey, message, signature) {
		return nil, errors.New("lebro/channels/discord: request signature mismatch")
	}
	return body, nil
}

// WebhookResponse responds to Discord's verified PING interaction.
func (*Adapter) WebhookResponse(_ *http.Request, body []byte) ([]byte, bool, error) {
	var interaction interaction
	if err := json.Unmarshal(body, &interaction); err != nil {
		return nil, false, fmt.Errorf("lebro/channels/discord: decode interaction: %w", err)
	}
	if interaction.Type != 1 {
		return nil, false, nil
	}
	return []byte(`{"type":1}`), true, nil
}

// Decode accepts application commands only. A command's channel is the stable
// durable conversation; Discord thread channels therefore remain distinct from
// their parent channels without a provider-specific mapping layer.
func (*Adapter) Decode(_ *http.Request, body []byte) (channels.InboundMessage, bool, error) {
	var interaction interaction
	if err := json.Unmarshal(body, &interaction); err != nil {
		return channels.InboundMessage{}, false, fmt.Errorf("lebro/channels/discord: decode interaction: %w", err)
	}
	if interaction.Type != 2 {
		return channels.InboundMessage{}, false, nil
	}
	user := interaction.User
	if interaction.Member.User.ID != "" {
		user = interaction.Member.User
	}
	if interaction.ID == "" || interaction.ApplicationID == "" || interaction.Token == "" || interaction.ChannelID == "" || user.ID == "" || interaction.Data.Name == "" {
		return channels.InboundMessage{}, false, errors.New("lebro/channels/discord: command is missing required identifiers")
	}
	text := "/" + interaction.Data.Name
	if arguments := commandArguments(interaction.Data.Options); arguments != "" {
		text += " " + arguments
	}
	metadata := map[string]string{
		"discord.application_id": interaction.ApplicationID,
		"discord.channel_id":     interaction.ChannelID,
		"discord.interaction_id": interaction.ID,
		"discord.command":        interaction.Data.Name,
	}
	if interaction.GuildID != "" {
		metadata["discord.guild_id"] = interaction.GuildID
	}
	return channels.InboundMessage{
		Conversation:      channels.ConversationRef{Platform: platform, ID: interaction.ChannelID, ReplyTarget: replyTarget(interaction.ApplicationID, interaction.Token)},
		ProviderMessageID: interaction.ID,
		Sender:            channels.ChannelIdentity{ProviderUserID: user.ID, DisplayName: user.Username, Tenant: interaction.GuildID},
		Text:              text,
		Metadata:          metadata,
	}, true, nil
}

// WebhookAcknowledgement defers a verified command after it has been accepted
// by Config.Dispatch. Discord shows its native loading state until Send edits
// the original interaction response.
func (*Adapter) WebhookAcknowledgement(_ *http.Request, _ []byte) ([]byte, string, error) {
	return []byte(`{"type":5}`), "application/json; charset=utf-8", nil
}

// Send ignores stream deltas and edits Discord's deferred original response
// when the final text is available. Longer replies are split at 2,000 runes:
// the first edits the original response and later chunks use followups.
func (a *Adapter) Send(ctx context.Context, conversation channels.ConversationRef, message channels.OutboundMessage) error {
	if !message.Final {
		return nil
	}
	if conversation.Platform != platform {
		return fmt.Errorf("lebro/channels/discord: cannot send to platform %q", conversation.Platform)
	}
	applicationID, token, ok := parseReplyTarget(conversation.ReplyTarget)
	if !ok {
		return errors.New("lebro/channels/discord: invalid reply target")
	}
	chunks := splitMessage(message.Text)
	for i, chunk := range chunks {
		path := "/webhooks/" + applicationID + "/" + token
		method := http.MethodPost
		if i == 0 {
			path += "/messages/@original"
			method = http.MethodPatch
		}
		if err := a.sendReply(ctx, method, path, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) sendReply(ctx context.Context, method, path, content string) error {
	payload, err := json.Marshal(discordMessage{Content: content, AllowedMentions: allowedMentions{Parse: []string{}}})
	if err != nil {
		return fmt.Errorf("lebro/channels/discord: encode reply: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.apiBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("lebro/channels/discord: create reply request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lebro/channels/discord: send reply: %w", err)
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if readErr != nil {
		return fmt.Errorf("lebro/channels/discord: read reply response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("lebro/channels/discord: close reply response: %w", closeErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("lebro/channels/discord: reply returned HTTP %d", response.StatusCode)
	}
	return nil
}

type interaction struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"`
	Token         string `json:"token"`
	GuildID       string `json:"guild_id"`
	ChannelID     string `json:"channel_id"`
	User          user   `json:"user"`
	Member        struct {
		User user `json:"user"`
	} `json:"member"`
	Data struct {
		Name    string          `json:"name"`
		Options []commandOption `json:"options"`
	} `json:"data"`
}

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type commandOption struct {
	Name    string          `json:"name"`
	Value   json.RawMessage `json:"value"`
	Options []commandOption `json:"options"`
}

type discordMessage struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
}

func commandArguments(options []commandOption) string {
	var values []string
	var collect func([]commandOption)
	collect = func(items []commandOption) {
		for _, option := range items {
			if len(option.Options) > 0 {
				collect(option.Options)
				continue
			}
			if len(option.Value) == 0 || string(option.Value) == "null" {
				continue
			}
			var value string
			if json.Unmarshal(option.Value, &value) != nil {
				value = string(option.Value)
			}
			values = append(values, value)
		}
	}
	collect(options)
	return strings.Join(values, " ")
}

func replyTarget(applicationID, token string) string { return applicationID + "\x00" + token }

func parseReplyTarget(value string) (applicationID, token string, ok bool) {
	parts := strings.Split(value, "\x00")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func splitMessage(text string) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	chunks := make([]string, 0, (len(runes)+maxMessageRunes-1)/maxMessageRunes)
	for len(runes) > 0 {
		end := maxMessageRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

var (
	_ channels.Adapter             = (*Adapter)(nil)
	_ channels.WebhookResponder    = (*Adapter)(nil)
	_ channels.WebhookAcknowledger = (*Adapter)(nil)
)
