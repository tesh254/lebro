package telegram

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tesh254/lebro/channels"
)

const (
	platform          = "telegram"
	defaultAPIBaseURL = "https://api.telegram.org"
	defaultMaxBody    = 1 << 20
	maxMessageRunes   = 4096
)

// Config configures a Telegram Bot API webhook adapter.
type Config struct {
	// SecretToken is the value configured through Telegram's setWebhook
	// secret_token option. It is verified against the inbound header.
	SecretToken string
	// BotToken authorizes outbound Bot API calls.
	BotToken     string
	HTTPClient   *http.Client
	APIBaseURL   string
	MaxBodyBytes int64
}

// Adapter is a concurrent-safe Telegram webhook adapter.
type Adapter struct {
	secretToken []byte
	botToken    string
	httpClient  *http.Client
	apiBaseURL  string
	maxBody     int64
}

// New constructs a Telegram adapter.
func New(config Config) (*Adapter, error) {
	if config.SecretToken == "" {
		return nil, errors.New("lebro/channels/telegram: SecretToken is required")
	}
	if config.BotToken == "" {
		return nil, errors.New("lebro/channels/telegram: BotToken is required")
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
		return nil, errors.New("lebro/channels/telegram: MaxBodyBytes must be positive")
	}
	return &Adapter{secretToken: []byte(config.SecretToken), botToken: config.BotToken, httpClient: config.HTTPClient, apiBaseURL: strings.TrimRight(config.APIBaseURL, "/"), maxBody: config.MaxBodyBytes}, nil
}

func (*Adapter) Platform() string { return platform }

// Verify compares the setWebhook secret token before parsing the update.
func (a *Adapter) Verify(r *http.Request) ([]byte, error) {
	if r == nil {
		return nil, errors.New("lebro/channels/telegram: request is nil")
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), a.secretToken) != 1 {
		return nil, errors.New("lebro/channels/telegram: request secret mismatch")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, a.maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("lebro/channels/telegram: read request body: %w", err)
	}
	if int64(len(body)) > a.maxBody {
		return nil, errors.New("lebro/channels/telegram: request body exceeds MaxBodyBytes")
	}
	return body, nil
}

// Decode accepts ordinary text message updates. Other update types are safely
// acknowledged without creating a user turn.
func (*Adapter) Decode(_ *http.Request, body []byte) (channels.InboundMessage, bool, error) {
	var update update
	if err := json.Unmarshal(body, &update); err != nil {
		return channels.InboundMessage{}, false, fmt.Errorf("lebro/channels/telegram: decode update: %w", err)
	}
	message := update.Message
	if update.UpdateID == 0 || message.MessageID == 0 || message.Chat.ID == 0 || message.From.ID == 0 || message.Text == "" {
		return channels.InboundMessage{}, false, nil
	}
	conversationID := strconv.FormatInt(message.Chat.ID, 10)
	if message.MessageThreadID != 0 {
		conversationID += "\x00" + strconv.FormatInt(message.MessageThreadID, 10)
	}
	return channels.InboundMessage{
		Conversation:      channels.ConversationRef{Platform: platform, ID: conversationID, ReplyTarget: replyTarget(message.Chat.ID, message.MessageThreadID, message.MessageID)},
		ProviderMessageID: strconv.FormatInt(update.UpdateID, 10),
		Sender:            channels.ChannelIdentity{ProviderUserID: strconv.FormatInt(message.From.ID, 10), DisplayName: message.From.Username, Tenant: strconv.FormatInt(message.Chat.ID, 10)},
		Text:              message.Text,
		Metadata:          map[string]string{"telegram.chat_id": strconv.FormatInt(message.Chat.ID, 10), "telegram.message_id": strconv.FormatInt(message.MessageID, 10)},
	}, true, nil
}

// Send sends only the completed reply. Replies longer than the Bot API limit
// are split without breaking UTF-8; the first is a native reply to the update.
func (a *Adapter) Send(ctx context.Context, conversation channels.ConversationRef, message channels.OutboundMessage) error {
	if !message.Final {
		return nil
	}
	if conversation.Platform != platform {
		return fmt.Errorf("lebro/channels/telegram: cannot send to platform %q", conversation.Platform)
	}
	chatID, topicID, messageID, ok := parseReplyTarget(conversation.ReplyTarget)
	if !ok {
		return errors.New("lebro/channels/telegram: invalid reply target")
	}
	for i, text := range splitMessage(message.Text) {
		payload := sendMessage{ChatID: chatID, Text: text}
		if topicID != 0 {
			payload.MessageThreadID = topicID
		}
		if i == 0 {
			payload.ReplyParameters = &replyParameters{MessageID: messageID}
		}
		if err := a.send(ctx, payload); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) send(ctx context.Context, payload sendMessage) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("lebro/channels/telegram: encode sendMessage: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/bot"+a.botToken+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("lebro/channels/telegram: create sendMessage: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lebro/channels/telegram: call sendMessage: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if readErr != nil {
		return fmt.Errorf("lebro/channels/telegram: read sendMessage response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("lebro/channels/telegram: close sendMessage response: %w", closeErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("lebro/channels/telegram: sendMessage returned HTTP %d", response.StatusCode)
	}
	var result apiResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("lebro/channels/telegram: decode sendMessage response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("lebro/channels/telegram: sendMessage: %s", result.Description)
	}
	return nil
}

type update struct {
	UpdateID int64   `json:"update_id"`
	Message  message `json:"message"`
}
type message struct {
	MessageID       int64  `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id"`
	Text            string `json:"text"`
	Chat            chat   `json:"chat"`
	From            user   `json:"from"`
}
type chat struct {
	ID int64 `json:"id"`
}
type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
type sendMessage struct {
	ChatID          int64            `json:"chat_id"`
	Text            string           `json:"text"`
	MessageThreadID int64            `json:"message_thread_id,omitempty"`
	ReplyParameters *replyParameters `json:"reply_parameters,omitempty"`
}
type replyParameters struct {
	MessageID int64 `json:"message_id"`
}
type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func replyTarget(chatID, topicID, messageID int64) string {
	return strconv.FormatInt(chatID, 10) + "\x00" + strconv.FormatInt(topicID, 10) + "\x00" + strconv.FormatInt(messageID, 10)
}
func parseReplyTarget(value string) (chatID, topicID, messageID int64, ok bool) {
	parts := strings.Split(value, "\x00")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if chatID, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
		return 0, 0, 0, false
	}
	if topicID, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return 0, 0, 0, false
	}
	if messageID, err = strconv.ParseInt(parts[2], 10, 64); err != nil || messageID == 0 {
		return 0, 0, 0, false
	}
	return chatID, topicID, messageID, true
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

var _ channels.Adapter = (*Adapter)(nil)
