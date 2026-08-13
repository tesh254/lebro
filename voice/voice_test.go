package voice_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/voice"
)

// echoModel is a minimal non-streaming model that replies with a fixed
// assistant turn. RunStream falls back to Generate for a Generate-only model, so
// it exercises the run path a voice turn drives without a streaming provider.
type echoModel struct {
	reply string
}

func (m echoModel) Generate(_ context.Context, req lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: m.reply},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func newAgent(t *testing.T, reply string) *lebro.Agent {
	t.Helper()
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "voice-agent", Name: "voice-agent"},
		Model:      echoModel{reply: reply},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

// fakeRecognizer drains the audio channel and emits a scripted sequence of
// transcripts, then reports done. It honors cancellation: when its context or
// stream is cancelled before it finishes, it stops and reports the cancel cause.
type fakeRecognizer struct {
	partials []string
	final    string
	// block, when non-nil, holds the recognizer open until it is closed or the
	// run is cancelled, so a test can cancel mid-recognition.
	block chan struct{}
	err   error
}

func (r fakeRecognizer) Recognize(ctx context.Context, audio <-chan voice.AudioChunk) (*voice.RecognitionStream, error) {
	transcripts := make(chan voice.Transcript)
	done := make(chan error, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(finished)
		defer close(done)
		defer close(transcripts)

		// Drain the input audio so the producer is never blocked.
		go func() {
			for range audio {
			}
		}()

		if r.err != nil {
			done <- r.err
			return
		}
		for _, p := range r.partials {
			select {
			case transcripts <- voice.Transcript{Text: p, Final: false}:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		if r.block != nil {
			select {
			case <-r.block:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		select {
		case transcripts <- voice.Transcript{Text: r.final, Final: true, Metadata: map[string]string{"lang": "en"}}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		done <- nil
	}()

	return voice.NewRecognitionStream(transcripts, done, finished, cancel), nil
}

// fakeSynthesizer emits one audio chunk per rune-run of the request text and a
// terminal chunk. It records the text it was asked to render so a test can
// assert the reply was synthesized.
type fakeSynthesizer struct {
	mu    sync.Mutex
	spoke string
	block chan struct{}
	err   error
}

func (s *fakeSynthesizer) Synthesize(ctx context.Context, req voice.SynthesisRequest) (*voice.SynthesisStream, error) {
	s.mu.Lock()
	s.spoke = req.Text
	s.mu.Unlock()

	chunks := make(chan voice.AudioChunk)
	done := make(chan error, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(finished)
		defer close(done)
		defer close(chunks)

		if s.err != nil {
			done <- s.err
			return
		}
		if s.block != nil {
			select {
			case <-s.block:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		select {
		case chunks <- voice.AudioChunk{Data: []byte(req.Text)}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		select {
		case chunks <- voice.AudioChunk{Final: true}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		done <- nil
	}()

	return voice.NewSynthesisStream(chunks, done, finished, cancel), nil
}

func (s *fakeSynthesizer) Speakers(context.Context) ([]voice.Speaker, error) {
	return []voice.Speaker{{ID: "default", Name: "Default"}}, nil
}

func (s *fakeSynthesizer) rendered() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spoke
}

var (
	_ voice.Recognizer  = fakeRecognizer{}
	_ voice.Synthesizer = (*fakeSynthesizer)(nil)
)

func audioOf(chunks ...string) <-chan voice.AudioChunk {
	ch := make(chan voice.AudioChunk, len(chunks)+1)
	for _, c := range chunks {
		ch <- voice.AudioChunk{Data: []byte(c)}
	}
	ch <- voice.AudioChunk{Final: true}
	close(ch)
	return ch
}

// TestTurnTranscribesRunsSynthesizes covers acceptance criterion 1: a transcript
// starts an agent run and the final response is synthesized.
func TestTurnTranscribesRunsSynthesizes(t *testing.T) {
	agent := newAgent(t, "hello back")
	synth := &fakeSynthesizer{}
	session, err := voice.NewSession(voice.SessionConfig{
		Voice: voice.Voice{
			Recognizer:  fakeRecognizer{partials: []string{"he", "hel"}, final: "hello there"},
			Synthesizer: synth,
		},
		Agent: agent,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	var got []byte
	result, err := session.Turn(context.Background(), voice.TurnInput{Audio: audioOf("aa", "bb")}, func(c voice.AudioChunk) error {
		got = append(got, c.Data...)
		return nil
	})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if result.Transcript.Text != "hello there" {
		t.Fatalf("transcript = %q, want %q", result.Transcript.Text, "hello there")
	}
	if result.Reply != "hello back" {
		t.Fatalf("reply = %q, want %q", result.Reply, "hello back")
	}
	if synth.rendered() != "hello back" {
		t.Fatalf("synthesized %q, want %q", synth.rendered(), "hello back")
	}
	if string(got) != "hello back" {
		t.Fatalf("delivered audio = %q, want %q", got, "hello back")
	}
	// The recognized transcript, not any partial, drove the run.
	if last := lastUser(result.Result); last != "hello there" {
		t.Fatalf("run user turn = %q, want %q", last, "hello there")
	}
}

func lastUser(result lebro.RunResult) string {
	for _, m := range result.Messages {
		if m.Role == lebro.RoleUser {
			return m.Content
		}
	}
	return ""
}

// TestCancelStopsTranscription covers acceptance criterion 2 for the recognition
// half: cancelling stops active transcription work and the stream reports the
// cancellation as a recognition failure.
func TestCancelStopsTranscription(t *testing.T) {
	agent := newAgent(t, "unused")
	rec := fakeRecognizer{partials: []string{"partial"}, final: "never", block: make(chan struct{})}
	session, err := voice.NewSession(voice.SessionConfig{
		Voice: voice.Voice{Recognizer: rec},
		Agent: agent,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := session.Transcribe(ctx, audioOf("x"))
		errc <- err
	}()
	cancel()

	err = <-errc
	if !errors.Is(err, voice.ErrRecognition) {
		t.Fatalf("Transcribe error = %v, want ErrRecognition", err)
	}
}

// TestCancelStopsSynthesis covers acceptance criterion 2 for the synthesis half.
func TestCancelStopsSynthesis(t *testing.T) {
	agent := newAgent(t, "unused")
	synth := &fakeSynthesizer{block: make(chan struct{})}
	session, err := voice.NewSession(voice.SessionConfig{
		Voice: voice.Voice{Synthesizer: synth},
		Agent: agent,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- session.Synthesize(ctx, voice.SynthesisRequest{Text: "hi"}, nil)
	}()
	cancel()

	err = <-errc
	if !errors.Is(err, voice.ErrSynthesis) {
		t.Fatalf("Synthesize error = %v, want ErrSynthesis", err)
	}
}

// TestVoiceFailureDistinctFromAgentFailure covers acceptance criterion 3: a
// voice provider failure is a *VoiceError and is never confused with an agent
// failure, while an agent failure keeps its own *lebro.AgentError.
func TestVoiceFailureDistinctFromAgentFailure(t *testing.T) {
	t.Run("recognition failure is a VoiceError, not an AgentError", func(t *testing.T) {
		session, err := voice.NewSession(voice.SessionConfig{
			Voice: voice.Voice{Recognizer: fakeRecognizer{err: errors.New("backend down")}},
			Agent: newAgent(t, "unused"),
		})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		_, err = session.Turn(context.Background(), voice.TurnInput{Audio: audioOf("x")}, nil)

		var voiceErr *voice.VoiceError
		if !errors.As(err, &voiceErr) {
			t.Fatalf("error %v is not a *VoiceError", err)
		}
		if voiceErr.Kind != voice.VoiceErrorRecognition {
			t.Fatalf("kind = %q, want recognition", voiceErr.Kind)
		}
		var agentErr *lebro.AgentError
		if errors.As(err, &agentErr) {
			t.Fatalf("recognition failure must not be an AgentError, got %v", agentErr)
		}
	})

	t.Run("agent failure keeps its AgentError, not wrapped as VoiceError", func(t *testing.T) {
		agent, err := lebro.NewAgent(lebro.AgentConfig{
			Definition: lebro.AgentDefinition{ID: "voice-agent", Name: "voice-agent"},
			Model:      failingModel{},
		})
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		synth := &fakeSynthesizer{}
		session, err := voice.NewSession(voice.SessionConfig{
			Voice: voice.Voice{
				Recognizer:  fakeRecognizer{final: "hi"},
				Synthesizer: synth,
			},
			Agent: agent,
		})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		_, err = session.Turn(context.Background(), voice.TurnInput{Audio: audioOf("x")}, nil)

		var agentErr *lebro.AgentError
		if !errors.As(err, &agentErr) {
			t.Fatalf("error %v is not a *lebro.AgentError", err)
		}
		var voiceErr *voice.VoiceError
		if errors.As(err, &voiceErr) {
			t.Fatalf("agent failure must not be a VoiceError, got %v", voiceErr)
		}
		if synth.rendered() != "" {
			t.Fatalf("synthesis must not run after an agent failure, rendered %q", synth.rendered())
		}
	})
}

// failingModel always fails the run so the agent returns an AgentError.
type failingModel struct{}

func (failingModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, &lebro.ModelError{Kind: lebro.ModelErrorUnavailable, Err: errors.New("provider down")}
}

// TestUnsupportedHalves covers a Voice with a missing half: the operation that
// needs it fails with a VoiceError of kind Unsupported rather than panicking.
func TestUnsupportedHalves(t *testing.T) {
	session, err := voice.NewSession(voice.SessionConfig{Voice: voice.Voice{}, Agent: newAgent(t, "unused")})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if _, err := session.Transcribe(context.Background(), audioOf("x")); !errors.Is(err, voice.ErrUnsupported) {
		t.Fatalf("Transcribe with no recognizer = %v, want ErrUnsupported", err)
	}
	if err := session.Synthesize(context.Background(), voice.SynthesisRequest{Text: "hi"}, nil); !errors.Is(err, voice.ErrUnsupported) {
		t.Fatalf("Synthesize with no synthesizer = %v, want ErrUnsupported", err)
	}
}

// TestTurnSinkWithoutSynthesizer covers the case where a caller supplies a sink
// but the Voice has no synthesizer: Turn must surface ErrUnsupported rather than
// silently returning the reply text and never invoking the sink.
func TestTurnSinkWithoutSynthesizer(t *testing.T) {
	session, err := voice.NewSession(voice.SessionConfig{
		Voice: voice.Voice{Recognizer: fakeRecognizer{final: "hi"}},
		Agent: newAgent(t, "reply"),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sinkCalled := false
	_, err = session.Turn(context.Background(), voice.TurnInput{Audio: audioOf("x")}, func(voice.AudioChunk) error {
		sinkCalled = true
		return nil
	})
	if !errors.Is(err, voice.ErrUnsupported) {
		t.Fatalf("Turn with sink and no synthesizer = %v, want ErrUnsupported", err)
	}
	if sinkCalled {
		t.Fatal("sink must not be called when no synthesizer is configured")
	}
}

// TestTurnNoSinkNoSynthesizer confirms the reply text is still returned when
// neither a sink nor a synthesizer is present: there is nothing to deliver, so
// the turn succeeds.
func TestTurnNoSinkNoSynthesizer(t *testing.T) {
	session, err := voice.NewSession(voice.SessionConfig{
		Voice: voice.Voice{Recognizer: fakeRecognizer{final: "hi"}},
		Agent: newAgent(t, "reply"),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	result, err := session.Turn(context.Background(), voice.TurnInput{Audio: audioOf("x")}, nil)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if result.Reply != "reply" {
		t.Fatalf("reply = %q, want %q", result.Reply, "reply")
	}
}

// TestNewSessionRequiresAgent guards the one construction invariant.
func TestNewSessionRequiresAgent(t *testing.T) {
	if _, err := voice.NewSession(voice.SessionConfig{}); err == nil {
		t.Fatal("NewSession with no agent must error")
	}
}
