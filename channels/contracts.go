package channels

import (
	"context"
	"net/http"

	"github.com/tesh254/lebro"
)

// ConversationRef identifies a conversation on a messaging platform. It is the
// provider's own thread or channel key — a Slack channel-plus-thread, a
// Telegram chat, a GitHub issue — carried verbatim so a ThreadMapper can derive
// a stable lebro ThreadID from it. The zero value names no conversation and is
// rejected by the handler.
type ConversationRef struct {
	// Platform is the adapter platform the conversation belongs to, such as
	// "slack" or "webhook". Two platforms may reuse the same provider key
	// without colliding because the mapper folds Platform into the thread ID.
	Platform string
	// ID is the provider's opaque conversation key. It is never parsed by the
	// core; a ThreadMapper hashes it to produce a deterministic ThreadID.
	ID string
}

// ChannelIdentity is the sender of an inbound message as the platform reports
// them. It is mapped onto a lebro.Identity for authorization and onto a thread
// owner for persistence, so a policy and a stored thread both see the same
// external principal.
//
// ProviderUserID is the platform's stable user key; DisplayName is optional
// human-readable text carried only for context. Tenant scopes the user for
// multi-tenant policies and is empty for single-tenant use.
type ChannelIdentity struct {
	ProviderUserID string
	DisplayName    string
	Tenant         string
}

// Identity projects the sender onto the runtime's authenticated-caller value
// for the given platform. The Subject scopes the provider user key by platform,
// because the same provider user ID on two platforms is two different callers; a
// bare provider ID as the subject would let a policy conflate them. The platform
// and display name are also preserved as attributes: platform for a policy that
// keys on it directly, and display name because the subject must stay stable.
// The fields are length-prefixed in the subject so a user ID that contains the
// separator cannot forge a different (platform, user) pair.
func (c ChannelIdentity) Identity(platform string) lebro.Identity {
	identity := lebro.Identity{
		Subject: string(lengthPrefixed(platform, c.ProviderUserID)),
		Tenant:  c.Tenant,
	}
	identity.Attributes = map[string]string{"platform": platform}
	if c.DisplayName != "" {
		identity.Attributes["display_name"] = c.DisplayName
	}
	return identity
}

// InboundMessage is one provider-neutral message received from a platform. An
// adapter's Decode produces it from a verified webhook request; the handler
// maps it to a thread, runs the agent, and streams the reply back through the
// adapter.
//
// ProviderMessageID is the platform's own message identifier and is the
// deduplication key: a platform that redelivers a webhook repeats this value,
// so the handler drops a second delivery that carries an ID it has already
// processed. An empty ProviderMessageID disables deduplication for that
// message, because there is nothing to key on.
type InboundMessage struct {
	// Conversation is the provider conversation the message belongs to. It is
	// mapped to a durable lebro thread so a reply continues the same
	// conversation.
	Conversation ConversationRef
	// ProviderMessageID is the platform's unique message key, used only for
	// deduplication. It is never persisted as content.
	ProviderMessageID string
	// Sender is the platform user who sent the message.
	Sender ChannelIdentity
	// Text is the message body delivered to the agent as a user turn.
	Text string
	// Metadata carries optional adapter context onto the run. It is passed
	// through to RunInput.Metadata and never interpreted by the core.
	Metadata map[string]string
}

// OutboundMessage is one unit of an agent reply delivered back to a
// conversation. A streamed run produces a sequence of chunks with Final false
// followed by exactly one chunk with Final true, so an adapter that only posts
// complete messages can ignore the non-final chunks and deliver the final one,
// while an adapter that supports live editing can render each chunk as it
// arrives.
type OutboundMessage struct {
	// Text is the reply content for this chunk. For a streamed reply it is the
	// incremental delta; for the final chunk an adapter may prefer the run's
	// complete assistant message, which the handler supplies as the final
	// chunk's Text.
	Text string
	// Final reports whether this is the terminal chunk of the reply. Exactly
	// one delivered chunk per reply has Final true.
	Final bool
	// Format is the rendering the adapter should apply to Text. It is copied
	// from the adapter's configured TextFormat so Send does not need to consult
	// the adapter for it.
	Format TextFormat
}

// TextFormat selects how an adapter renders reply text. Agents write markdown;
// FormatPlain requests literal plain text for platforms without markdown
// rendering.
type TextFormat string

const (
	// FormatMarkdown renders replies as markdown. It is the zero value and the
	// default, matching how agents write.
	FormatMarkdown TextFormat = ""
	// FormatPlain renders replies as literal plain text.
	FormatPlain TextFormat = "plain"
)

// Adapter is a provider-neutral messaging channel. One Adapter binds a single
// platform to a single agent: it verifies inbound webhook requests, decodes
// them into a neutral InboundMessage, and delivers agent replies back to the
// conversation. The core supplies the run pipeline, thread mapping, and
// deduplication; an Adapter supplies only the platform-specific edges.
//
// Implementations must be safe for concurrent use: a platform may deliver
// several webhooks at once, and the handler calls Verify, Decode, and Send from
// separate request goroutines.
type Adapter interface {
	// Platform reports the adapter's platform identifier, such as "slack" or
	// "webhook". It appears in the webhook route (/agents/{id}/channels/
	// {platform}/webhook) and in every ConversationRef the adapter decodes, so
	// it must be stable and URL-safe.
	Platform() string

	// Verify authenticates a webhook request before its body is trusted. It
	// returns nil when the request is authentic and an error otherwise; the
	// handler rejects an unverified request with 401 and never decodes it. An
	// adapter that needs the body to verify (for example, an HMAC over the raw
	// payload) reads and buffers it here and returns the buffered bytes for
	// Decode, so the body is read exactly once.
	Verify(r *http.Request) (body []byte, err error)

	// Decode parses a verified request body into a neutral InboundMessage. The
	// body is the exact bytes Verify returned. A request that carries no
	// actionable message — a platform challenge handshake, a delivery receipt —
	// returns ok false with a nil error so the handler acknowledges it with 200
	// without running the agent.
	Decode(r *http.Request, body []byte) (message InboundMessage, ok bool, err error)

	// Send delivers one reply chunk to the conversation. The handler calls it
	// for each streamed chunk in order and once more for the final chunk. An
	// adapter that cannot post incremental updates delivers only the final
	// chunk. Send must not retain the OutboundMessage past the call.
	Send(ctx context.Context, conversation ConversationRef, message OutboundMessage) error
}

// WebhookResponder is an optional extension for platforms whose verified
// webhook handshake requires a response body. The Server calls it after
// Adapter.Verify and before Adapter.Decode. When handled is true, it writes the
// returned body with HTTP 200 and does not invoke the agent.
//
// Slack's url_verification handshake is one example: its challenge is trusted
// only after the request signature has been checked, then echoed verbatim.
type WebhookResponder interface {
	WebhookResponse(r *http.Request, body []byte) (response []byte, handled bool, err error)
}

// DispatchJob is verified, deduplicated inbound work ready for a background
// worker. It contains only provider-neutral values, so a dispatcher may encode
// it into a durable queue. A worker resumes it with [Server.RunDispatch].
type DispatchJob struct {
	AgentID  string
	Platform string
	Message  InboundMessage
}

// DispatchFunc accepts verified, deduplicated channel work for execution. It
// must return only after durable acceptance when the platform needs a prompt
// webhook acknowledgement. A worker resumes the accepted job with
// [Server.RunDispatch].
//
// Leaving Config.Dispatch nil preserves synchronous request handling. A
// dispatcher that runs work asynchronously should detach cancellation from the
// inbound HTTP request and report failures through its own operational path.
type DispatchFunc func(ctx context.Context, job DispatchJob) error
