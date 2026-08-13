package voice

import (
	"github.com/tesh254/lebro"
)

// TranscriptMessage projects a recognized transcript onto a canonical lebro
// message. The role is fixed to user so a speaker's audio cannot forge a system
// or assistant turn the model would treat as its own prior output, matching how
// the channels, httpapi, and mcp adapters map external input. Only the
// transcript text becomes content; recognition metadata is carried separately
// onto the run input, not folded into the message body.
func TranscriptMessage(t Transcript) lebro.Message {
	return lebro.Message{Role: lebro.RoleUser, Content: t.Text}
}

// AssistantText returns the last assistant turn in a completed run, or empty
// when the transcript has none. It is the text a caller synthesizes as the
// spoken reply, and it never leaks a tool or system turn. It mirrors the same
// projection used by the channels and httpapi adapters, so a voice reply and a
// text reply speak the same words.
func AssistantText(result lebro.RunResult) string {
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == lebro.RoleAssistant {
			return result.Messages[i].Content
		}
	}
	return ""
}

// runMetadata merges a transcript's recognition metadata with any caller
// metadata for the run. Caller metadata takes precedence on a key collision, so
// a provider-reported field cannot silently override a value the caller set
// deliberately. It returns nil when there is nothing to carry, matching an
// unset RunInput.Metadata.
func runMetadata(transcript Transcript, base map[string]string) map[string]string {
	if len(transcript.Metadata) == 0 && len(base) == 0 {
		return nil
	}
	merged := make(map[string]string, len(transcript.Metadata)+len(base))
	for k, v := range transcript.Metadata {
		merged[k] = v
	}
	for k, v := range base {
		merged[k] = v
	}
	return merged
}
