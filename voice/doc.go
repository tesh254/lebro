// Package voice adds optional speech input and output around a lebro agent.
//
// A spoken utterance is transcribed to a canonical user turn; the agent runs
// through the ordinary streaming pipeline; the agent's final reply is
// synthesized back to audio. The package supplies the provider-neutral edges —
// speech-recognition and synthesis interfaces, streaming with cancellation, and
// translation to and from canonical messages — and leaves the provider-specific
// work to optional adapters. Adding voice changes no core orchestration: a
// [Session] drives the same [github.com/tesh254/lebro.Agent.RunStream] any other
// caller uses.
//
// # Model
//
// Two interfaces carry the provider work. A [Recognizer] consumes a stream of
// [AudioChunk] values and produces a stream of [Transcript] values
// (speech-to-text); a [Synthesizer] renders a [SynthesisRequest] as a stream of
// audio chunks (text-to-speech) and reports its available voices through
// [Synthesizer.Speakers]. A [Voice] bundles an optional recognizer and an
// optional synthesizer, so a listen-only or speak-only deployment leaves the
// other half nil.
//
// A [Session] binds a Voice to an agent. [Session.Turn] performs one voice turn
// end to end: it transcribes the audio, starts the agent run from the final
// transcript, and synthesizes the reply. It is a single-turn primitive; a
// continuous experience is a loop over Turn, which keeps the transport (a
// microphone, a phone bridge, a WebSocket) outside this package. [Session.Transcribe]
// and [Session.Synthesize] expose the two halves separately for callers that
// need finer control.
//
// # Canonical messages
//
// Only a final transcript starts a run, and it always maps to a user turn
// through [TranscriptMessage], so a speaker's audio cannot forge a system or
// assistant turn — the same rule the channels, httpapi, and mcp adapters
// enforce for external input. Recognition metadata (detected language, speaker
// label) is carried onto the run's metadata rather than folded into the message
// body. The agent's final assistant turn is projected back to text with
// [AssistantText], so the synthesized reply and a text reply speak the same
// words.
//
// # Streaming and cancellation
//
// Recognition and synthesis are streamed: [RecognitionStream] and
// [SynthesisStream] follow the same ownership contract as the agent runtime's
// stream. The caller drains the result channel to completion, then calls Wait to
// collect the terminal error, and calls Cancel to stop early. Cancelling the
// context passed to a Session method — or cancelling a stream directly — stops
// the active transcription or synthesis and joins the provider goroutine, so an
// abandoned turn leaves nothing running. Within [Session.Turn], a cancelled
// context stops whichever stage is active: transcription, the agent run, or
// synthesis.
//
// # Distinct failures
//
// A voice provider failure is always distinguishable from an agent failure. A
// recognition or synthesis fault is a [*VoiceError] whose Kind is
// [VoiceErrorRecognition] or [VoiceErrorSynthesis], matchable with
// [ErrRecognition] or [ErrSynthesis]; an operation on a missing half of a Voice
// is a VoiceError of kind [VoiceErrorUnsupported]. An agent-run failure keeps
// its own [github.com/tesh254/lebro.AgentError] and is never wrapped in a
// VoiceError, so a caller separates a provider outage from an agent fault with
// errors.As.
//
// # Providers
//
// The package ships no concrete provider: it stays provider-neutral, and a
// speech backend is an external adapter that implements [Recognizer] and/or
// [Synthesizer]. This keeps a concrete speech dependency out of the module,
// mirroring how model and storage adapters live outside the core.
//
// # Realtime
//
// This package covers the request/response integration points — a turn
// transcribes, runs, and synthesizes. A full-duplex realtime transport, where a
// provider streams audio in and out over a persistent connection and drives the
// turn boundary itself, is a separate adapter concern layered on these
// interfaces and is deliberately out of scope here.
package voice
