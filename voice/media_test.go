package voice_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/voice"
)

type mediaSTT struct{}

func (mediaSTT) StreamTranscription(ctx context.Context, r lebro.LiveTranscriptionRequest, source lebro.AudioSource, sink func(lebro.TranscriptionEvent) error) error {
	b := make([]byte, 32<<10)
	for {
		_, e := source(ctx, b)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
	}
	if e := sink(lebro.TranscriptionEvent{Segment: lebro.TranscriptSegment{ID: "s", Text: "hel"}}); e != nil {
		return e
	}
	return sink(lebro.TranscriptionEvent{Segment: lebro.TranscriptSegment{ID: "s", Text: "hello"}, Final: true, Terminal: true})
}

type mediaTTS struct{}

func (mediaTTS) StreamSpeech(ctx context.Context, r lebro.SpeechRequest, sink func(lebro.MediaAsset, []byte) error) (lebro.MediaResultInfo, error) {
	return lebro.MediaResultInfo{Operation: r.Operation}, sink(lebro.MediaAsset{Kind: lebro.MediaAudio, MIMEType: "audio/mpeg"}, []byte(r.Text))
}
func TestMediaVoiceBridgeTurn(t *testing.T) {
	v, e := voice.NewMediaVoice(voice.MediaVoiceConfig{Transcriber: mediaSTT{}, Speech: mediaTTS{}, Voices: []voice.Speaker{{ID: "alloy"}}})
	if e != nil {
		t.Fatal(e)
	}
	session, e := voice.NewSession(voice.SessionConfig{Voice: v, Agent: newAgent(t, "spoken reply")})
	if e != nil {
		t.Fatal(e)
	}
	audio := make(chan voice.AudioChunk, 1)
	audio <- voice.AudioChunk{Data: make([]byte, 4800), Final: true}
	close(audio)
	var got []byte
	result, e := session.Turn(context.Background(), voice.TurnInput{Audio: audio, Voice: "alloy"}, func(c voice.AudioChunk) error { got = append(got, c.Data...); return nil })
	if e != nil {
		t.Fatal(e)
	}
	if result.Transcript.Text != "hello" || string(got) != "spoken reply" {
		t.Fatalf("bad voice turn %+v %q", result, got)
	}
	speakers, e := v.Synthesizer.Speakers(context.Background())
	if e != nil || len(speakers) != 1 {
		t.Fatal(e)
	}
	if _, e = v.Synthesizer.Synthesize(context.Background(), voice.SynthesisRequest{Format: voice.AudioFormat{Channels: 2}}); e == nil {
		t.Fatal("silently converted audio")
	}
}
func TestMediaVoiceCancellation(t *testing.T) {
	v, e := voice.NewMediaVoice(voice.MediaVoiceConfig{Transcriber: mediaSTT{}})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, e := v.Recognizer.Recognize(ctx, make(chan voice.AudioChunk))
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	for range stream.Transcripts {
	}
	if e = stream.Wait(); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

type speakerSTT struct{}

func (speakerSTT) StreamTranscription(ctx context.Context, r lebro.LiveTranscriptionRequest, source lebro.AudioSource, sink func(lebro.TranscriptionEvent) error) error {
	if e := sink(lebro.TranscriptionEvent{Segment: lebro.TranscriptSegment{ID: "s", Text: "partial", Speaker: "agent"}}); e != nil {
		return e
	}
	return sink(lebro.TranscriptionEvent{Segment: lebro.TranscriptSegment{ID: "s", Text: "final text", Speaker: "agent"}, Final: true, Terminal: true})
}
func TestMediaVoiceTranscriptMetadataAndTerminal(t *testing.T) {
	v, e := voice.NewMediaVoice(voice.MediaVoiceConfig{Transcriber: speakerSTT{}})
	if e != nil {
		t.Fatal(e)
	}
	audio := make(chan voice.AudioChunk)
	close(audio)
	stream, e := v.Recognizer.Recognize(context.Background(), audio)
	if e != nil {
		t.Fatal(e)
	}
	var last voice.Transcript
	for tr := range stream.Transcripts {
		last = tr
	}
	if e = stream.Wait(); e != nil {
		t.Fatal(e)
	}
	if !last.Final || last.Text != "final text" || last.Metadata["speaker"] != "agent" {
		t.Fatalf("bad transcript %+v", last)
	}
}
func TestMediaVoiceTypedNilProviders(t *testing.T) {
	var nilSTT lebro.StreamingTranscriber = (*mediaSTT)(nil)
	var nilTTS lebro.StreamingSpeechSynthesizer = (*mediaTTS)(nil)
	if _, e := voice.NewMediaVoice(voice.MediaVoiceConfig{Transcriber: nilSTT}); e == nil {
		t.Fatal("typed-nil transcriber accepted")
	}
	if _, e := voice.NewMediaVoice(voice.MediaVoiceConfig{Speech: nilTTS}); e == nil {
		t.Fatal("typed-nil speech accepted")
	}
	if _, e := voice.NewMediaVoice(voice.MediaVoiceConfig{}); e == nil {
		t.Fatal("no provider accepted")
	}
	v, e := voice.NewMediaVoice(voice.MediaVoiceConfig{Transcriber: nilSTT, Speech: mediaTTS{}})
	if e != nil {
		t.Fatal(e)
	}
	if v.Recognizer != nil || v.Synthesizer == nil {
		t.Fatal("wrong bridge installation")
	}
}
