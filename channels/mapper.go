package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/tesh254/lebro"
)

// ThreadMapper resolves an inbound message to the durable thread its
// conversation continues. The mapping must be deterministic: the same
// conversation must always yield the same ThreadID so a reply and every later
// message land on one persisted transcript. The returned OwnerID and Namespace
// scope the thread when the runtime lazily creates it.
//
// Implementations must be pure and safe for concurrent use; they are called on
// every inbound message.
type ThreadMapper interface {
	Map(message InboundMessage) (id lebro.ThreadID, namespace string, ownerID string)
}

// NamespaceThreadMapper derives a deterministic ThreadID by hashing the
// message's platform, namespace, and provider conversation key. Folding the
// platform and a caller-chosen namespace into the hash keeps two platforms — or
// two logically separate deployments sharing one store — from colliding on the
// same provider conversation key.
//
// The sender's provider user key becomes the thread OwnerID, mirroring the
// resource/thread split messaging runtimes use: the owner is the external
// principal, the thread is the conversation. A shared multi-user conversation
// therefore maps to one thread whose owner is the most recent sender; callers
// that need strict per-user threads fold the user key into the conversation ID
// before constructing the message.
type NamespaceThreadMapper struct {
	// Namespace scopes every thread this mapper produces. It is stored on the
	// thread record and mixed into the ID hash. An empty namespace is valid and
	// yields an unscoped mapping.
	Namespace string
}

// Map returns the deterministic thread ID, namespace, and owner for a message.
func (m NamespaceThreadMapper) Map(message InboundMessage) (lebro.ThreadID, string, string) {
	// Fields are length-prefixed before hashing so no field's contents can
	// shift a boundary and alias a different split: a raw delimiter fails when a
	// field legitimately contains that delimiter byte, but a length prefix
	// cannot be forged from the field's own bytes.
	sum := sha256.Sum256(lengthPrefixed(m.Namespace, message.Conversation.Platform, message.Conversation.ID))
	return lebro.ThreadID("ch-" + hex.EncodeToString(sum[:])), m.Namespace, message.Sender.ProviderUserID
}

// ErrNoConversation is returned by validateInbound when an inbound message
// names no conversation, so it cannot be mapped to a thread. It is a client
// error, not a server fault: an adapter that decodes a message must populate
// the conversation reference.
var ErrNoConversation = errors.New("lebro/channels: inbound message has no conversation reference")

// validateInbound checks that a decoded message can be mapped to a thread. The
// handler rejects a message that fails this with 400, and the returned sentinel
// lets a caller that decodes messages itself branch on the cause.
func validateInbound(message InboundMessage) error {
	if message.Conversation.ID == "" {
		return ErrNoConversation
	}
	return nil
}
