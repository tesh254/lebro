package voice

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"reflect"

	"github.com/tesh254/lebro"
)

// MediaVoiceConfig adapts first-class media providers to the existing Session.
// Recognition input must be 24kHz mono PCM16 when using OpenAI live STT.
type MediaVoiceConfig struct {
	Transcriber lebro.StreamingTranscriber
	Speech      lebro.StreamingSpeechSynthesizer
	Operation   func(context.Context) (lebro.MediaOperation, error)
	Voices      []Speaker
}
type mediaVoice struct{ config MediaVoiceConfig }

// NewMediaVoice bridges media adapters without changing existing Voice contracts.
// For an editable prompt field, call Transcribe/StreamTranscription directly;
// Session.Turn intentionally starts an agent run from the finalized transcript.
func NewMediaVoice(c MediaVoiceConfig) (Voice, error) {
	if (c.Transcriber == nil || isNilProvider(c.Transcriber)) && (c.Speech == nil || isNilProvider(c.Speech)) {
		return Voice{}, errors.New("lebro/voice: a media provider is required")
	}
	if c.Operation == nil {
		c.Operation = func(ctx context.Context) (lebro.MediaOperation, error) {
			scope, _ := lebro.RuntimeScopeFromContext(ctx)
			return lebro.MediaOperation{ID: rand.Text(), Scope: scope}, nil
		}
	}
	c.Voices = append([]Speaker(nil), c.Voices...)
	for i, s := range c.Voices {
		m := map[string]string{}
		for k, v := range s.Metadata {
			m[k] = v
		}
		c.Voices[i].Metadata = m
	}
	bridge := &mediaVoice{config: c}
	v := Voice{}
	if c.Transcriber != nil && !isNilProvider(c.Transcriber) {
		v.Recognizer = bridge
	}
	if c.Speech != nil && !isNilProvider(c.Speech) {
		v.Synthesizer = bridge
	}
	return v, nil
}

func isNilProvider(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}
func (v *mediaVoice) Recognize(ctx context.Context, audio <-chan AudioChunk) (*RecognitionStream, error) {
	o, err := v.config.Operation(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	transcripts := make(chan Transcript)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer close(done)
		defer close(transcripts)
		defer cancel()
		ended := false
		source := func(ctx context.Context, b []byte) (int, error) {
			if ended {
				return 0, io.EOF
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case c, ok := <-audio:
				if !ok {
					return 0, io.EOF
				}
				if len(c.Data) > len(b) {
					return 0, errors.New("lebro/voice: media audio chunks must be at most 32 KiB")
				}
				n := copy(b, c.Data)
				if c.Final {
					ended = true
					return n, io.EOF
				}
				return n, nil
			}
		}
		err := v.config.Transcriber.StreamTranscription(ctx, lebro.LiveTranscriptionRequest{Operation: o}, source, func(e lebro.TranscriptionEvent) error {
			t := Transcript{Text: e.Segment.Text, Final: e.Final && e.Terminal}
			if e.Segment.Confidence != nil {
				t.Confidence = *e.Segment.Confidence
			}
			if e.Segment.Speaker != "" {
				t.Metadata = map[string]string{"speaker": e.Segment.Speaker}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case transcripts <- t:
				return nil
			}
		})
		done <- err
	}()
	return NewRecognitionStream(transcripts, done, finished, cancel), nil
}
func (v *mediaVoice) Synthesize(ctx context.Context, r SynthesisRequest) (*SynthesisStream, error) {
	if r.Format.SampleRate != 0 || r.Format.Channels != 0 {
		return nil, errors.New("lebro/voice: explicit sample-rate/channel conversion is unsupported")
	}
	o, err := v.config.Operation(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	chunks := make(chan AudioChunk)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer close(done)
		defer close(chunks)
		defer cancel()
		_, err := v.config.Speech.StreamSpeech(ctx, lebro.SpeechRequest{Operation: o, Text: r.Text, Voice: r.Voice, Format: r.Format.Encoding}, func(_ lebro.MediaAsset, b []byte) error {
			chunk := AudioChunk{Data: append([]byte(nil), b...)}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case chunks <- chunk:
				return nil
			}
		})
		if err == nil {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case chunks <- AudioChunk{Final: true}:
			}
		}
		done <- err
	}()
	return NewSynthesisStream(chunks, done, finished, cancel), nil
}
func (v *mediaVoice) Speakers(ctx context.Context) ([]Speaker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Speaker, len(v.config.Voices))
	for i, s := range v.config.Voices {
		out[i] = s
		out[i].Metadata = map[string]string{}
		for k, val := range s.Metadata {
			out[i].Metadata[k] = val
		}
	}
	return out, nil
}
