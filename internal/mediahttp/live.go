package mediahttp

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tesh254/lebro"
)

// StreamTranscription accepts one 24kHz, mono, little-endian PCM16 utterance.
// End-of-input commits it. A final segment replaces all provisional text.
func (c *Client) StreamTranscription(ctx context.Context, r lebro.LiveTranscriptionRequest, source lebro.AudioSource, sink func(lebro.TranscriptionEvent) error) (retErr error) {
	if err := c.authorize(ctx, r.Operation, "media.generate"); err != nil {
		return err
	}
	start := time.Now().UTC()
	defer func() {
		retErr = errors.Join(retErr, c.record(ctx, c.info(r.Operation, nil), r.Operation, "live_transcription", start, retErr))
	}()
	if !c.caps.StreamingTranscription {
		return unsupported("live transcription is unsupported")
	}
	if err := r.Operation.Validate(); err != nil {
		return err
	}
	if source == nil || sink == nil {
		return invalid("audio source and transcript consumer are required")
	}
	if r.Language != "" && !c.caps.Language || r.Prompt != "" && !c.caps.PromptHint {
		return unsupported("unsupported transcription option")
	}
	if len(r.Language) > 16 || len(r.Prompt) > 4096 {
		return invalid("over-limit transcription hints")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	address := strings.Replace(strings.Replace(c.base, "https://", "wss://", 1), "http://", "ws://", 1) + "/realtime?intent=transcription"
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 30 * time.Second
	conn, resp, err := dialer.DialContext(ctx, address, http.Header{"Authorization": []string{"Bearer " + c.key}})
	if err != nil {
		if resp != nil {
			return responseError(resp)
		}
		return &lebro.MediaError{Kind: lebro.MediaErrorTransport, Message: "transcription connection failed", Cause: err}
	}
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(1 << 20)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	transcription := map[string]any{"model": c.model}
	if r.Language != "" {
		transcription["languages"] = []string{r.Language}
	}
	if r.Prompt != "" {
		transcription["prompt"] = r.Prompt
	}
	if err = conn.WriteJSON(map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription", "audio": map[string]any{"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "transcription": transcription, "turn_detection": nil}}}}); err != nil {
		return malformed("transcription session setup failed")
	}
	// Wait for configuration acknowledgement so rejected sessions consume no audio.
	for {
		var event struct {
			Type string `json:"type"`
		}
		_ = conn.SetReadDeadline(time.Now().Add(c.timeout))
		if err = conn.ReadJSON(&event); err != nil {
			return liveError(ctx, err)
		}
		if event.Type == "error" {
			return malformed("transcription configuration rejected")
		}
		if event.Type == "session.updated" {
			break
		}
	}
	done := make(chan error, 1)
	var committed atomic.Bool
	go func() {
		var e error
		defer func() {
			done <- e
			if e != nil {
				_ = conn.Close()
			}
		}()
		buf := make([]byte, 32<<10)
		var total int64
		for {
			if e = ctx.Err(); e != nil {
				return
			}
			var n int
			n, e = source(ctx, buf)
			if n < 0 || n > len(buf) || n%2 != 0 {
				e = invalid("PCM chunks must contain complete 16-bit samples")
				return
			}
			total += int64(n)
			if total > c.caps.MaxInputBytes {
				e = invalid("audio exceeds byte limit")
				return
			}
			if n > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(c.timeout))
				if we := conn.WriteJSON(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(buf[:n])}); we != nil {
					e = we
					return
				}
			}
			if errors.Is(e, io.EOF) {
				if total < 4800 {
					e = invalid("live transcription requires at least 100ms of PCM audio")
					return
				}
				committed.Store(true)
				e = conn.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"})
				return
			}
			if e != nil {
				return
			}
			if n == 0 {
				e = io.ErrNoProgress
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = conn.Close()
		if sourceErr := <-done; sourceErr != nil && !errors.Is(sourceErr, context.Canceled) {
			retErr = sourceErr
		}
	}()
	text := ""
	segmentID := ""
	for {
		var event struct {
			Type       string `json:"type"`
			ItemID     string `json:"item_id"`
			Delta      string `json:"delta"`
			Transcript string `json:"transcript"`
		}
		_ = conn.SetReadDeadline(time.Now().Add(c.timeout))
		if err = conn.ReadJSON(&event); err != nil {
			return liveError(ctx, err)
		}
		switch event.Type {
		case "error", "conversation.item.input_audio_transcription.failed":
			return &lebro.MediaError{Kind: lebro.MediaErrorRemote, Message: "live transcription failed"}
		case "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed":
			if event.ItemID == "" || !safeID(event.ItemID) {
				return malformed("transcript has invalid segment identity")
			}
			if segmentID != "" && segmentID != event.ItemID {
				return malformed("unexpected second utterance")
			}
			segmentID = event.ItemID
			final := event.Type == "conversation.item.input_audio_transcription.completed"
			if final && !committed.Load() {
				return malformed("transcription finalized before end-of-input")
			}
			if final {
				text = event.Transcript
			} else {
				text += event.Delta
			}
			if len(text) > 1<<20 {
				return malformed("transcript exceeds text limit")
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = sink(lebro.TranscriptionEvent{Segment: lebro.TranscriptSegment{ID: segmentID, Text: text}, Final: final, Terminal: final}); err != nil {
				return err
			}
			if final {
				return nil
			}
		}
	}
}
func liveError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &lebro.MediaError{Kind: lebro.MediaErrorTransport, Message: "transcription disconnected before finalization", Cause: err}
}
