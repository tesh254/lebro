package channels_test

import (
	"testing"

	"github.com/tesh254/lebro/channels"
)

func TestNamespaceThreadMapperIsDeterministic(t *testing.T) {
	mapper := channels.NamespaceThreadMapper{Namespace: "prod"}
	message := channels.InboundMessage{
		Conversation: channels.ConversationRef{Platform: "webhook", ID: "conv-1"},
		Sender:       channels.ChannelIdentity{ProviderUserID: "user-9"},
	}

	first, ns, owner := mapper.Map(message)
	second, _, _ := mapper.Map(message)
	if first != second {
		t.Fatalf("same message mapped to different threads: %q vs %q", first, second)
	}
	if first == "" {
		t.Fatal("mapped thread ID is empty")
	}
	if ns != "prod" {
		t.Fatalf("namespace = %q, want prod", ns)
	}
	if owner != "user-9" {
		t.Fatalf("owner = %q, want user-9", owner)
	}
}

func TestNamespaceThreadMapperSeparatesConversations(t *testing.T) {
	mapper := channels.NamespaceThreadMapper{Namespace: "prod"}
	a, _, _ := mapper.Map(channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "webhook", ID: "conv-1"}})
	b, _, _ := mapper.Map(channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "webhook", ID: "conv-2"}})
	if a == b {
		t.Fatal("distinct conversations mapped to the same thread")
	}
}

func TestNamespaceThreadMapperSeparatesPlatformsAndNamespaces(t *testing.T) {
	byPlatformA, _, _ := channels.NamespaceThreadMapper{Namespace: "prod"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "slack", ID: "x"}})
	byPlatformB, _, _ := channels.NamespaceThreadMapper{Namespace: "prod"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "teams", ID: "x"}})
	if byPlatformA == byPlatformB {
		t.Fatal("same conversation ID on different platforms collided")
	}

	byNamespaceA, _, _ := channels.NamespaceThreadMapper{Namespace: "a"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "slack", ID: "x"}})
	byNamespaceB, _, _ := channels.NamespaceThreadMapper{Namespace: "b"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "slack", ID: "x"}})
	if byNamespaceA == byNamespaceB {
		t.Fatal("same conversation in different namespaces collided")
	}
}

// TestNamespaceThreadMapperResistsBoundaryShift proves the length prefix
// matters: with a raw delimiter, ("ab","c") and ("a","bc") could hash
// identically, and a field containing the delimiter byte could alias another
// split.
func TestNamespaceThreadMapperResistsBoundaryShift(t *testing.T) {
	a, _, _ := channels.NamespaceThreadMapper{Namespace: "ab"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "p", ID: "c"}})
	b, _, _ := channels.NamespaceThreadMapper{Namespace: "a"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "bp", ID: "c"}})
	if a == b {
		t.Fatal("boundary shift produced a collision")
	}

	// A conversation ID that contains the NUL byte a naive delimiter would use
	// must not alias a different platform/ID split.
	withNul, _, _ := channels.NamespaceThreadMapper{Namespace: "n"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "p", ID: "a\x00b"}})
	shifted, _, _ := channels.NamespaceThreadMapper{Namespace: "n"}.Map(
		channels.InboundMessage{Conversation: channels.ConversationRef{Platform: "p\x00a", ID: "b"}})
	if withNul == shifted {
		t.Fatal("NUL byte in a field aliased a different split")
	}
}

func TestChannelIdentityScopesSubjectByPlatform(t *testing.T) {
	sender := channels.ChannelIdentity{ProviderUserID: "u1", DisplayName: "Ann"}
	slack := sender.Identity("slack")
	teams := sender.Identity("teams")

	if slack.Subject == teams.Subject {
		t.Fatalf("same user ID on different platforms produced the same subject %q", slack.Subject)
	}
	if slack.Attributes["platform"] != "slack" {
		t.Fatalf("platform attribute = %q, want slack", slack.Attributes["platform"])
	}
	if slack.Attributes["display_name"] != "Ann" {
		t.Fatalf("display_name attribute = %q, want Ann", slack.Attributes["display_name"])
	}
}
