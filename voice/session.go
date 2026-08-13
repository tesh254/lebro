package voice

import (
	"context"
	"errors"

	"github.com/tesh254/lebro"
)

// Session wires a Voice to an agent for one voice turn: it transcribes inbound
// audio, starts the agent run from the final transcript, and synthesizes the
// agent's final reply as outbound audio. It is a single-turn primitive — one
// call performs recognize → run → synthesize — so a continuous voice experience
// is built by calling it in a loop, keeping the orchestration core free of any
// transport concern.
//
// A Session holds no per-call state and is safe for concurrent use; each Turn
// runs independently against the shared Voice and Agent.
type Session struct {
	voice Voice
	agent *lebro.Agent
}

// SessionConfig configures a Session.
type SessionConfig struct {
	// Voice supplies the recognizer and synthesizer. Either half may be nil; a
	// turn that needs a missing half fails with a VoiceError of kind
	// Unsupported.
	Voice Voice
	// Agent is the agent a transcript starts a run on. It is required.
	Agent *lebro.Agent
}

// NewSession returns a Session bound to the configured voice and agent. It
// errors when no agent is configured, since a session with no agent cannot run
// a turn.
func NewSession(config SessionConfig) (*Session, error) {
	if config.Agent == nil {
		return nil, errors.New("lebro/voice: session requires an agent")
	}
	return &Session{voice: config.Voice, agent: config.Agent}, nil
}

// TurnInput carries the inputs for one voice turn.
type TurnInput struct {
	// Audio delivers the caller's spoken utterance as a stream of audio chunks.
	// The caller sends chunks and closes the channel when the utterance ends.
	Audio <-chan AudioChunk
	// ThreadID continues a durable conversation across turns. Empty runs an
	// unthreaded turn.
	ThreadID lebro.ThreadID
	// Format is the audio encoding to synthesize the reply in. The zero value
	// lets the synthesizer choose its default.
	Format AudioFormat
	// Voice is the synthesis voice ID for the reply, chosen from
	// Synthesizer.Speakers. Empty uses the provider default.
	Voice string
	// Metadata is carried onto the run alongside the transcript's own metadata;
	// on a key collision this value wins.
	Metadata map[string]string
}

// TurnResult is the outcome of one voice turn.
type TurnResult struct {
	// Transcript is the final recognized text that started the run.
	Transcript Transcript
	// Result is the completed agent run.
	Result lebro.RunResult
	// Reply is the last assistant turn, the text that was synthesized. It is
	// empty when the run produced no assistant turn.
	Reply string
}

// Transcribe consumes the audio stream and returns the final transcript,
// draining partials. It fails with a VoiceError of kind Unsupported when the
// session has no recognizer, and of kind Recognition when the provider fails or
// the context is cancelled. Cancelling ctx stops the active transcription: the
// recognizer's stream is cancelled and its goroutine joined before returning.
func (s *Session) Transcribe(ctx context.Context, audio <-chan AudioChunk) (Transcript, error) {
	if s.voice.Recognizer == nil {
		return Transcript{}, newVoiceError(VoiceErrorUnsupported, errors.New("no recognizer configured"))
	}
	stream, err := s.voice.Recognizer.Recognize(ctx, audio)
	if err != nil {
		return Transcript{}, newVoiceError(VoiceErrorRecognition, err)
	}
	// Cancel unconditionally so an early return never leaves the recognizer
	// goroutine parked writing to a channel nobody reads.
	defer stream.Cancel()

	var final Transcript
	var seenFinal bool
	for t := range stream.Transcripts {
		if t.Final {
			final = t
			seenFinal = true
		}
	}
	if err := stream.Wait(); err != nil {
		return Transcript{}, newVoiceError(VoiceErrorRecognition, err)
	}
	if !seenFinal {
		return Transcript{}, newVoiceError(VoiceErrorRecognition, errors.New("recognition produced no final transcript"))
	}
	return final, nil
}

// Synthesize renders text as audio and delivers each chunk to sink in order.
// It fails with a VoiceError of kind Unsupported when the session has no
// synthesizer, and of kind Synthesis when the provider fails, the context is
// cancelled, or sink returns an error. Cancelling ctx stops the active
// synthesis. A nil sink drains the audio without delivering it.
func (s *Session) Synthesize(ctx context.Context, request SynthesisRequest, sink func(AudioChunk) error) error {
	if s.voice.Synthesizer == nil {
		return newVoiceError(VoiceErrorUnsupported, errors.New("no synthesizer configured"))
	}
	stream, err := s.voice.Synthesizer.Synthesize(ctx, request)
	if err != nil {
		return newVoiceError(VoiceErrorSynthesis, err)
	}
	defer stream.Cancel()

	var sinkErr error
	for chunk := range stream.Chunks {
		if sinkErr != nil {
			// A sink already failed; keep draining so the provider goroutine can
			// finish, but stop delivering. The deferred Cancel unblocks it.
			continue
		}
		if sink == nil {
			continue
		}
		if err := sink(chunk); err != nil {
			sinkErr = err
			stream.Cancel()
		}
	}
	if err := stream.Wait(); err != nil {
		return newVoiceError(VoiceErrorSynthesis, err)
	}
	if sinkErr != nil {
		return newVoiceError(VoiceErrorSynthesis, sinkErr)
	}
	return nil
}

// Turn performs one full voice turn: transcribe the audio, start an agent run
// from the final transcript, and synthesize the reply to sink. It returns the
// transcript, the run result, and the synthesized reply text.
//
// The three failure sources stay distinct. A recognition or synthesis failure
// is a *VoiceError (kind Recognition or Synthesis); an agent-run failure keeps
// its own *lebro.AgentError. So a caller can tell a provider outage apart from
// an agent fault with errors.As, satisfying the guarantee that voice provider
// failures are distinct from agent failures.
//
// Cancelling ctx stops whichever stage is active — transcription, the run, or
// synthesis — and joins its goroutine before returning, so an abandoned turn
// leaves nothing running. A nil sink runs the turn and reports the reply text
// without emitting audio.
func (s *Session) Turn(ctx context.Context, input TurnInput, sink func(AudioChunk) error) (TurnResult, error) {
	transcript, err := s.Transcribe(ctx, input.Audio)
	if err != nil {
		return TurnResult{}, err
	}

	runInput := lebro.RunInput{
		Messages: []lebro.Message{TranscriptMessage(transcript)},
		ThreadID: input.ThreadID,
		Metadata: runMetadata(transcript, input.Metadata),
	}

	run, err := s.agent.RunStream(ctx, runInput)
	if err != nil {
		// An agent failure is not wrapped in a VoiceError: it keeps its
		// AgentError so a caller distinguishes it from a provider fault.
		return TurnResult{}, err
	}
	// Drain to completion so the run goroutine is not left blocked, then collect
	// the terminal result. Drain also cancels on the caller's cancelled context
	// via the run's own context wiring.
	result, runErr := run.Drain()
	if runErr != nil {
		return TurnResult{}, runErr
	}

	reply := AssistantText(result)
	if s.voice.Synthesizer != nil {
		req := SynthesisRequest{Text: reply, Format: input.Format, Voice: input.Voice}
		if err := s.Synthesize(ctx, req, sink); err != nil {
			return TurnResult{}, err
		}
	}

	return TurnResult{Transcript: transcript, Result: result, Reply: reply}, nil
}
