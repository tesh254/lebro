// voice-booking is the reservations-line build: spoken turns transcribe in,
// run the agent, and synthesize back out — and the booking conversation is
// remembered per caller across calls through durable threads.
//
// The recognizer and synthesizer are in-file fakes standing in for the
// provider adapters a real deployment supplies; the thread persistence and
// voice session wiring are all library.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/voice"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	store := lebro.NewMemoryStore()
	agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "host",
			Name:         "Host",
			Instructions: "Run the reservations line.",
		},
		Model: bookingModel{},
		Store: store,
	}))

	session := mustValue(voice.NewSession(voice.SessionConfig{Voice: voice.Voice{
		Recognizer:  fakeRecognizer{},
		Synthesizer: fakeSynthesizer{},
	}, Agent: agent}))

	// First call from one caller: a new booking.
	speak(output, session, "caller-555", "Book a table for two on Friday at seven")
	// The same caller calls back; the thread continues the booking context.
	speak(output, session, "caller-555", "Add a high chair to that booking")
	// A different caller starts fresh.
	speak(output, session, "caller-777", "Do you have outdoor seating")

	for _, threadID := range []string{"caller-555", "caller-777"} {
		page, err := store.Messages().ListMessages(context.Background(), lebro.ThreadID(threadID), lebro.PageRequest{})
		if err != nil {
			return err
		}
		writef(output, "%s persisted turns: %d\n", threadID, len(page.Records))
	}
	return nil
}

// speak runs one voice turn for a caller and prints what was heard and said.
func speak(output io.Writer, session *voice.Session, callerThreadID, utterance string) {
	audio := make(chan voice.AudioChunk, 2)
	audio <- voice.AudioChunk{Data: []byte(utterance)}
	audio <- voice.AudioChunk{Final: true}
	close(audio)

	result, err := session.Turn(context.Background(), voice.TurnInput{
		Audio:    audio,
		ThreadID: lebro.ThreadID(callerThreadID),
	}, func(voice.AudioChunk) error { return nil })
	if err != nil {
		panic(err)
	}
	writef(output, "\n[%s] heard: %q\n[%s] said:  %s\n", callerThreadID, result.Transcript.Text, callerThreadID, result.Reply)
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

// bookingModel plays the host agent's provider. It answers from the whole
// thread the run loaded: one prior user turn means a new booking; more than
// one means an existing booking is being changed.
type bookingModel struct{}

func (bookingModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	prior := 0
	latest := ""
	for _, message := range request.Messages {
		if message.Role != lebro.RoleUser {
			continue
		}
		prior++
		latest = message.Content
	}
	var reply string
	switch {
	case strings.Contains(strings.ToLower(latest), "high chair"):
		reply = "Done - a high chair is added to your Friday table for two."
	case strings.Contains(strings.ToLower(latest), "outdoor"):
		reply = "Yes, outdoor seating is first come from five o'clock."
	case prior > 1:
		reply = "I have updated your existing Friday booking."
	default:
		reply = "Table for two, Friday at seven, under this number - confirmed."
	}
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: reply},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

// fakeRecognizer transcribes by joining the audio chunks' bytes as text and
// emitting one partial then the final transcript. See examples/voice for the
// same stand-in annotated in full.
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
		select {
		case transcripts <- voice.Transcript{Text: firstHalfRunes(text), Final: false}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		select {
		case transcripts <- voice.Transcript{Text: text, Final: true, Confidence: 0.93}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		done <- nil
	}()

	return voice.NewRecognitionStream(transcripts, done, finished, cancel), nil
}

func firstHalfRunes(text string) string {
	runes := []rune(text)
	return string(runes[:len(runes)/2])
}

// fakeSynthesizer renders text as one audio chunk of its bytes plus a terminal
// chunk.
type fakeSynthesizer struct{}

func (fakeSynthesizer) Synthesize(ctx context.Context, req voice.SynthesisRequest) (*voice.SynthesisStream, error) {
	chunks := make(chan voice.AudioChunk, 2)
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
	return []voice.Speaker{{ID: "host", Name: "Host"}}, nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
