package voice

import (
	"errors"
	"fmt"
)

// VoiceErrorKind classifies a voice failure so a caller can branch on the stage
// that failed without matching on error strings.
type VoiceErrorKind string

const (
	// VoiceErrorRecognition marks a failure in speech-to-text: the recognizer
	// could not transcribe the audio, or the recognition was cancelled.
	VoiceErrorRecognition VoiceErrorKind = "recognition"
	// VoiceErrorSynthesis marks a failure in text-to-speech: the synthesizer
	// could not render the text, or the synthesis was cancelled.
	VoiceErrorSynthesis VoiceErrorKind = "synthesis"
	// VoiceErrorUnsupported marks an operation a Voice cannot perform because the
	// required half is nil — a recognition on a Voice with no Recognizer, or a
	// synthesis on a Voice with no Synthesizer.
	VoiceErrorUnsupported VoiceErrorKind = "unsupported"
)

// Sentinels let a caller match a voice failure by kind with errors.Is. They are
// distinct from the runtime's agent-run sentinels (ErrAgent*), so a voice
// provider failure is always distinguishable from a failure of the agent it
// wraps: errors.Is(err, ErrRecognition) is true only for a recognition fault,
// never for an agent fault, and vice versa.
var (
	// ErrRecognition matches any VoiceError of kind Recognition.
	ErrRecognition = errors.New("lebro/voice: recognition failed")
	// ErrSynthesis matches any VoiceError of kind Synthesis.
	ErrSynthesis = errors.New("lebro/voice: synthesis failed")
	// ErrUnsupported matches any VoiceError of kind Unsupported.
	ErrUnsupported = errors.New("lebro/voice: operation not supported by voice")
)

// VoiceError reports a failure originating in a voice provider or in the voice
// wiring, as opposed to a failure of the agent run itself. Keeping voice faults
// in their own type lets a caller separate a provider outage (a recognizer that
// cannot reach its backend, a synthesizer that rejects a format) from an agent
// failure (a tool error, a step-limit exhaustion): the run's own error keeps its
// AgentError type, while everything the voice layer contributes is a
// *VoiceError.
type VoiceError struct {
	// Kind classifies the failing stage.
	Kind VoiceErrorKind
	// Err is the underlying cause, wrapped for errors.Is/errors.As.
	Err error
}

// Error renders the kind and the wrapped cause.
func (e *VoiceError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("lebro/voice: %s error", e.Kind)
	}
	return fmt.Sprintf("lebro/voice: %s error: %v", e.Kind, e.Err)
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *VoiceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target is the sentinel for this error's kind, so
// errors.Is(err, ErrRecognition) matches a recognition VoiceError regardless of
// its wrapped cause.
func (e *VoiceError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrRecognition:
		return e.Kind == VoiceErrorRecognition
	case ErrSynthesis:
		return e.Kind == VoiceErrorSynthesis
	case ErrUnsupported:
		return e.Kind == VoiceErrorUnsupported
	default:
		return false
	}
}

// newVoiceError wraps cause in a *VoiceError of the given kind. It returns nil
// when cause is nil so it composes with a plain error return.
func newVoiceError(kind VoiceErrorKind, cause error) error {
	if cause == nil {
		return nil
	}
	return &VoiceError{Kind: kind, Err: cause}
}
