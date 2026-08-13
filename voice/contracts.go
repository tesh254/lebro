package voice

import (
	"context"
)

// AudioFormat describes the encoding of a stream of audio samples. It travels
// with both recognition input and synthesis output so a provider knows how to
// decode inbound audio and how to render outbound audio. The zero value names
// no format; a provider that requires one rejects it.
type AudioFormat struct {
	// Encoding is the sample encoding, such as "pcm_s16le", "mp3", or "opus".
	// It is an opaque provider-neutral token: the core never parses it, and a
	// Recognizer or Synthesizer matches it against the formats it supports.
	Encoding string
	// SampleRate is the number of samples per second, such as 16000 or 24000.
	// Zero means unspecified, letting a provider apply its own default.
	SampleRate int
	// Channels is the number of interleaved audio channels; 1 is mono, 2 is
	// stereo. Zero means unspecified.
	Channels int
}

// AudioChunk is one segment of an audio stream. Recognition consumes a sequence
// of chunks as its input; synthesis produces a sequence of chunks as its
// output. A streamed sequence carries incremental chunks with Final false
// followed by exactly one chunk with Final true, so a consumer that only needs
// the complete audio can wait for the terminal chunk while a consumer that
// renders progressively can act on each chunk as it arrives.
type AudioChunk struct {
	// Data is the raw audio bytes for this chunk, encoded per the stream's
	// AudioFormat. A provider must not retain the slice past the call that
	// delivered it; the caller owns the backing array and may reuse it. A
	// provider that keeps the bytes beyond the call must copy them first.
	Data []byte
	// Final reports whether this is the terminal chunk of the stream. Exactly
	// one chunk per stream has Final true; it may carry no Data.
	Final bool
}

// Transcript is one provider-neutral speech-recognition result. A streamed
// recognition emits a sequence of partial transcripts with Final false as the
// speaker talks, followed by exactly one transcript with Final true carrying
// the settled text. Only a final transcript starts an agent run; partials are
// for live display.
type Transcript struct {
	// Text is the recognized text for this result. For a partial it is the
	// best-so-far hypothesis; for the final it is the settled transcription.
	Text string
	// Final reports whether the recognizer considers this text settled. Exactly
	// one emitted transcript per recognition has Final true.
	Final bool
	// Confidence is the recognizer's optional confidence in Text, in [0,1]. A
	// provider that reports none leaves it zero, which is indistinguishable from
	// a genuine zero score, so treat zero as "unreported" unless a provider
	// documents otherwise.
	Confidence float64
	// Metadata carries optional provider context (detected language, speaker
	// label) onto the run. It is passed through to RunInput.Metadata and never
	// interpreted by the core.
	Metadata map[string]string
}

// Speaker identifies a synthesis voice a Synthesizer can render. It is returned
// by Speakers so a caller can discover the available voices and pass a chosen
// ID as SynthesisRequest.Voice.
type Speaker struct {
	// ID is the provider's stable voice identifier, passed back as
	// SynthesisRequest.Voice to select this voice.
	ID string
	// Name is optional human-readable text for the voice, carried only for
	// display.
	Name string
	// Metadata carries optional provider attributes (language, gender) for the
	// voice. It is never interpreted by the core.
	Metadata map[string]string
}

// SynthesisRequest is one request to render text as speech.
type SynthesisRequest struct {
	// Text is the content to synthesize. It is typically the agent's final
	// assistant turn, projected with AssistantText.
	Text string
	// Format is the audio encoding the synthesizer should produce. The zero
	// value lets the provider choose its default.
	Format AudioFormat
	// Voice is the provider voice ID to render with, chosen from Speakers. Empty
	// lets the provider use its default voice.
	Voice string
}

// Recognizer is a provider-neutral speech-to-text adapter. It consumes a stream
// of audio chunks and produces a stream of transcripts. It is an optional
// adapter: the core supplies the run pipeline and the canonical-message
// translation, and a Recognizer supplies only the provider-specific
// transcription.
//
// Implementations must be safe for concurrent use; a caller may run several
// recognitions at once.
type Recognizer interface {
	// Recognize starts transcribing the audio delivered on audio and returns a
	// RecognitionStream carrying the resulting transcripts. The caller sends
	// audio chunks on audio and closes it when the utterance ends; the
	// implementation must stop reading audio when ctx is cancelled or the stream
	// is cancelled, so an abandoned recognition does not leak a goroutine. The
	// implementation must not retain an AudioChunk's Data past the read that
	// delivered it.
	Recognize(ctx context.Context, audio <-chan AudioChunk) (*RecognitionStream, error)
}

// Synthesizer is a provider-neutral text-to-speech adapter. It renders text as
// a stream of audio chunks. Like Recognizer it is an optional adapter carrying
// only the provider-specific synthesis.
//
// Implementations must be safe for concurrent use.
type Synthesizer interface {
	// Synthesize starts rendering request.Text as speech and returns a
	// SynthesisStream carrying the resulting audio chunks. The implementation
	// must stop producing audio when ctx is cancelled or the stream is
	// cancelled.
	Synthesize(ctx context.Context, request SynthesisRequest) (*SynthesisStream, error)
	// Speakers reports the voices this synthesizer can render, so a caller can
	// discover a voice ID to pass as SynthesisRequest.Voice. A provider with a
	// single fixed voice may return an empty slice.
	Speakers(ctx context.Context) ([]Speaker, error)
}

// Voice bundles an optional Recognizer and an optional Synthesizer into one
// value, so a caller can carry both halves of a voice experience together and
// leave either unconfigured. A Session accepts a Voice and uses whichever half
// a step needs; a nil half causes the operation that needs it to fail with a
// VoiceError of kind Unsupported rather than a nil-pointer panic, so a
// listen-only or speak-only deployment is expressed by leaving the other half
// nil.
type Voice struct {
	// Recognizer transcribes inbound audio. Nil disables recognition.
	Recognizer Recognizer
	// Synthesizer renders outbound audio. Nil disables synthesis.
	Synthesizer Synthesizer
}
