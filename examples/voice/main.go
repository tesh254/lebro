// voice demonstrates a full voice turn: an audio utterance is transcribed to a
// user turn, the agent runs through the ordinary pipeline, and the final reply
// is synthesized back to audio.
//
// The example is dependency-free: it uses an in-file fake recognizer and
// synthesizer and a scripted model, so it runs without an API key or a real
// speech backend. The fakes stand in for the optional provider adapters a real
// deployment would supply.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/voice"
)

// scriptedModel replies with a fixed assistant turn. RunStream falls back to
// Generate for a Generate-only model, so the voice turn runs without a streaming
// provider. It stands in for a real model adapter.
type scriptedModel struct{}

func (scriptedModel) Generate(_ context.Context, req lebro.ModelRequest) (lebro.ModelResponse, error) {
	// Echo the recognized text back so the spoken reply reflects the input.
	var heard string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == lebro.RoleUser {
			heard = req.Messages[i].Content
			break
		}
	}
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: "You said: " + heard},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// fakeRecognizer transcribes by concatenating the audio chunks' bytes as text.
// It emits one partial and one final transcript. A real recognizer would call a
// speech backend; this keeps the example self-contained.
type fakeRecognizer struct{}

func (fakeRecognizer) Recognize(ctx context.Context, audio <-chan voice.AudioChunk) (*voice.RecognitionStream, error) {
	transcripts := make(chan voice.Transcript)
	done := make(chan error, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(finished)
		defer close(done)
		defer close(transcripts)

		var text string
		for chunk := range audio {
			text += string(chunk.Data)
		}

		// A partial for live display, then the settled final that starts the run.
		select {
		case transcripts <- voice.Transcript{Text: text[:len(text)/2], Final: false}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		select {
		case transcripts <- voice.Transcript{Text: text, Final: true, Confidence: 0.94}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		done <- nil
	}()

	return voice.NewRecognitionStream(transcripts, done, finished, cancel), nil
}

// fakeSynthesizer renders text as one audio chunk of its bytes and a terminal
// chunk. A real synthesizer would produce encoded audio frames.
type fakeSynthesizer struct{}

func (fakeSynthesizer) Synthesize(ctx context.Context, req voice.SynthesisRequest) (*voice.SynthesisStream, error) {
	chunks := make(chan voice.AudioChunk)
	done := make(chan error, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(finished)
		defer close(done)
		defer close(chunks)

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

func (fakeSynthesizer) Speakers(context.Context) ([]voice.Speaker, error) {
	return []voice.Speaker{{ID: "narrator", Name: "Narrator"}}, nil
}

func main() {
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "assistant", Name: "assistant"},
		Model:      scriptedModel{},
	})
	if err != nil {
		log.Fatalf("new agent: %v", err)
	}

	session, err := voice.NewSession(voice.SessionConfig{
		Voice: voice.Voice{
			Recognizer:  fakeRecognizer{},
			Synthesizer: fakeSynthesizer{},
		},
		Agent: agent,
	})
	if err != nil {
		log.Fatalf("new session: %v", err)
	}

	// The caller's spoken utterance, delivered as audio chunks.
	audio := make(chan voice.AudioChunk, 3)
	audio <- voice.AudioChunk{Data: []byte("what is ")}
	audio <- voice.AudioChunk{Data: []byte("the weather")}
	audio <- voice.AudioChunk{Final: true}
	close(audio)

	// Collect the synthesized reply audio as it is delivered.
	var reply []byte
	result, err := session.Turn(context.Background(), voice.TurnInput{
		Audio: audio,
		Voice: "narrator",
	}, func(chunk voice.AudioChunk) error {
		reply = append(reply, chunk.Data...)
		return nil
	})
	if err != nil {
		log.Fatalf("voice turn: %v", err)
	}

	fmt.Printf("transcript: %q (confidence %.2f)\n", result.Transcript.Text, result.Transcript.Confidence)
	fmt.Printf("reply text: %q\n", result.Reply)
	fmt.Printf("synthesized audio bytes: %d (%q)\n", len(reply), reply)
}
