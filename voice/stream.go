package voice

import (
	"errors"
	"sync"
)

// RecognitionStream carries the ordered transcripts of an in-flight
// recognition. It mirrors the ownership contract of the agent runtime's stream:
// the stream owns the Transcripts channel, the caller drains it until it is
// closed, then calls Wait to collect the terminal error. The caller must call
// Cancel when it stops reading before the stream completes, so the provider
// goroutine is not left blocked writing to a channel nobody reads.
type RecognitionStream struct {
	// Transcripts delivers the recognition's transcripts in order. It is closed
	// when recognition ends, whether by completion, cancellation, or error.
	Transcripts <-chan Transcript

	done     chan error
	finished chan struct{}
	cancel   func()
	once     sync.Once
}

// SynthesisStream carries the ordered audio chunks of an in-flight synthesis.
// Its contract matches RecognitionStream: drain Chunks to completion, then call
// Wait; call Cancel to stop early.
type SynthesisStream struct {
	// Chunks delivers the synthesized audio in order. It is closed when
	// synthesis ends, whether by completion, cancellation, or error.
	Chunks <-chan AudioChunk

	done     chan error
	finished chan struct{}
	cancel   func()
	once     sync.Once
}

// NewRecognitionStream wires a RecognitionStream for a Recognizer to return.
// The recognizer owns transcripts and closes it when recognition ends; it sends
// its terminal error (or nil) exactly once on done and closes finished when its
// goroutine exits, so Wait can report the outcome and join the goroutine. cancel
// stops the recognition and must unblock the goroutine; it may be nil when the
// recognizer needs no teardown beyond context cancellation.
//
// A recognizer typically starts a goroutine that reads audio, writes to the
// transcripts channel, and on exit sends its result on done and closes both
// transcripts and finished.
func NewRecognitionStream(transcripts <-chan Transcript, done chan error, finished chan struct{}, cancel func()) *RecognitionStream {
	return &RecognitionStream{Transcripts: transcripts, done: done, finished: finished, cancel: cancel}
}

// NewSynthesisStream wires a SynthesisStream for a Synthesizer to return,
// following NewRecognitionStream's contract over the audio channel.
func NewSynthesisStream(chunks <-chan AudioChunk, done chan error, finished chan struct{}, cancel func()) *SynthesisStream {
	return &SynthesisStream{Chunks: chunks, done: done, finished: finished, cancel: cancel}
}

// Cancel stops the active recognition and releases its goroutine. It is safe to
// call multiple times and must be invoked when the caller stops reading
// Transcripts before the stream completes naturally.
func (s *RecognitionStream) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Wait blocks until the recognition goroutine has completed and returns its
// terminal error, or nil on success. It must be called after Transcripts has
// been fully drained so the goroutine is not blocked writing to the channel. On
// cancellation it returns a VoiceError of kind Recognition wrapping the
// cancellation cause.
func (s *RecognitionStream) Wait() error {
	if s == nil {
		return errors.New("lebro/voice: recognition stream is nil")
	}
	err, ok := <-s.done
	if !ok {
		return errors.New("lebro/voice: recognition ended without outcome")
	}
	<-s.finished
	return err
}

// Cancel stops the active synthesis and releases its goroutine. It is safe to
// call multiple times.
func (s *SynthesisStream) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Wait blocks until the synthesis goroutine has completed and returns its
// terminal error, or nil on success. It must be called after Chunks has been
// fully drained. On cancellation it returns a VoiceError of kind Synthesis
// wrapping the cancellation cause.
func (s *SynthesisStream) Wait() error {
	if s == nil {
		return errors.New("lebro/voice: synthesis stream is nil")
	}
	err, ok := <-s.done
	if !ok {
		return errors.New("lebro/voice: synthesis ended without outcome")
	}
	<-s.finished
	return err
}
