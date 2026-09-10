package mediahttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tesh254/lebro"
)

func operation() lebro.MediaOperation { return lebro.MediaOperation{ID: "operation-1"} }
func adapter(t *testing.T, provider, model string, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, e := New(Config{APIKey: "fixture-key", BaseURL: s.URL, Model: model}, provider)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func pngData() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3)))
	return b.Bytes()
}
func wavData() []byte { return append([]byte("RIFF0000WAVE"), make([]byte, 4800)...) }
func TestImageWireAndPartial(t *testing.T) {
	for _, p := range []string{"openai", "openrouter"} {
		t.Run(p, func(t *testing.T) {
			c := adapter(t, p, "gpt-image-1", func(w http.ResponseWriter, r *http.Request) {
				path := "/images/generations"
				if p == "openrouter" {
					path = "/images"
				}
				if r.URL.Path != path || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("wrong transport: %s", r.URL.Path)
				}
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				if in["prompt"] != "draw" || in["n"] != float64(2) {
					t.Errorf("payload: %v", in)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData()), "revised_prompt": "revised"}}, "usage": map[string]any{"cost": 0, "total_tokens": 3}})
			})
			out, e := c.GenerateImages(context.Background(), lebro.ImageRequest{Operation: operation(), Prompt: "draw", Count: 2})
			if e != nil {
				t.Fatal(e)
			}
			if !out.Partial || len(out.Images) != 1 || out.Images[0].Asset.Width != 2 || out.Images[0].Asset.Height != 3 || out.Info.Usage.CostUSD == nil || out.Info.Usage.CostUSD.String() != "0" {
				t.Fatalf("bad result: %+v", out)
			}
		})
	}
}
func TestImageRejectsBeforeSubmission(t *testing.T) {
	var calls atomic.Int32
	c := adapter(t, "openai", "gpt-image-1", func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, r := range []lebro.ImageRequest{{Prompt: "draw"}, {Operation: operation()}, {Operation: operation(), Prompt: "draw", Count: 11}, {Operation: operation(), Prompt: "draw", Size: "bad"}, {Operation: operation(), Prompt: "draw", Format: "svg"}, {Operation: operation(), Prompt: "draw", Quality: "bad"}, {Operation: operation(), Prompt: "draw", Resolution: "4K"}} {
		if _, e := c.GenerateImages(context.Background(), r); e == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("submitted invalid request")
	}
}
func TestImageMalformed(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":[]}`, `{"data":[{"b64_json":"broken"}]}`, `{"data":[{"b64_json":"eA=="}]}`, `{"data":[{"url":"https://secret.example/?key=secret"}]}`, `{bad`} {
		t.Run(body, func(t *testing.T) {
			c := adapter(t, "openai", "gpt-image-1", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
			if _, e := c.GenerateImages(context.Background(), lebro.ImageRequest{Operation: operation(), Prompt: "draw"}); e == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
}
func TestErrorNormalizationAndRedaction(t *testing.T) {
	for status, kind := range map[int]lebro.MediaErrorKind{400: lebro.MediaErrorValidation, 401: lebro.MediaErrorAuthentication, 403: lebro.MediaErrorAuthorization, 429: lebro.MediaErrorRateLimit, 503: lebro.MediaErrorRemote, 410: lebro.MediaErrorExpired} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := adapter(t, "openai", "gpt-image-1", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"secret-prompt secret-key","code":"secret-code"}}`)
			})
			_, e := c.GenerateImages(context.Background(), lebro.ImageRequest{Operation: operation(), Prompt: "draw"})
			var me *lebro.MediaError
			if !errors.As(e, &me) || me.Kind != kind || strings.Contains(e.Error(), "secret") {
				t.Fatalf("error: %v", e)
			}
			if me.RetryAfter != 2*time.Second {
				t.Fatal("lost retry hint")
			}
		})
	}
	c := adapter(t, "openai", "gpt-image-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":"content_policy_violation"}}`)
	})
	_, e := c.GenerateImages(context.Background(), lebro.ImageRequest{Operation: operation(), Prompt: "draw"})
	var me *lebro.MediaError
	if !errors.As(e, &me) || me.Kind != lebro.MediaErrorRefusal {
		t.Fatal(e)
	}
}
func TestTranscriptionMultipart(t *testing.T) {
	for _, provider := range []string{"openai", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			c := adapter(t, provider, "whisper-1", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/audio/transcriptions" {
					t.Error(r.URL.Path)
				}
				if e := r.ParseMultipartForm(1 << 20); e != nil {
					t.Error(e)
					return
				}
				defer func() { _ = r.MultipartForm.RemoveAll() }()
				f, _, e := r.FormFile("file")
				if e != nil {
					t.Error(e)
					return
				}
				defer func() { _ = f.Close() }()
				b, _ := io.ReadAll(f)
				if !bytes.Equal(b, wavData()) || r.FormValue("response_format") != "verbose_json" {
					t.Error("bad upload")
				}
				w.Header().Set("X-Generation-Id", "gen-1")
				_, _ = io.WriteString(w, `{"text":"hello","language":"en","duration":0,"segments":[{"id":4,"text":"hello","start":0,"end":1,"speaker":0}],"words":[{"word":"hello","start":0,"end":1}],"usage":{"seconds":0,"cost":0.00123}}`)
			})
			out, e := c.Transcribe(context.Background(), lebro.TranscriptionRequest{Operation: operation(), Audio: lebro.MediaContent{Asset: lebro.MediaAsset{Kind: lebro.MediaAudio, MIMEType: "audio/wav"}, Data: wavData()}, Timestamps: true})
			if e != nil {
				t.Fatal(e)
			}
			if out.Text != "hello" || out.DurationSeconds == nil || *out.DurationSeconds != 0 || out.Segments[0].Speaker != "0" || out.Segments[0].Confidence != nil || out.Info.ProviderRequestID != "gen-1" || out.Info.Usage.CostUSD.String() != "0.00123" {
				t.Fatalf("bad transcript: %+v", out)
			}
		})
	}
}
func TestTranscriptionEmptyAndInvalid(t *testing.T) {
	var calls atomic.Int32
	c := adapter(t, "openrouter", "whisper-1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"text":""}`)
	})
	r := lebro.TranscriptionRequest{Operation: operation(), Audio: lebro.MediaContent{Asset: lebro.MediaAsset{Kind: lebro.MediaAudio, MIMEType: "audio/wav"}, Data: []byte("corrupt")}}
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("accepted corrupt header")
	}
	r.Audio.Data = wavData()
	r.Prompt = "ignored by OpenRouter"
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("silently dropped prompt")
	}
	if calls.Load() != 0 {
		t.Fatal("submitted invalid input")
	}
	r.Prompt = ""
	out, e := c.Transcribe(context.Background(), r)
	if e != nil || out.Text != "" {
		t.Fatalf("silence: %+v %v", out, e)
	}
}
func TestSpeechStreamsOrderedBytesAndStops(t *testing.T) {
	payload := bytes.Repeat([]byte("audio"), 20000)
	c := adapter(t, "openrouter", "openai/gpt-4o-mini-tts", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["input"] != "hello" || in["voice"] != "alloy" || in["response_format"] != "mp3" {
			t.Errorf("bad speech request %v", in)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	})
	r := lebro.SpeechRequest{Operation: operation(), Text: "hello", Voice: "alloy"}
	var got bytes.Buffer
	chunks := 0
	_, e := c.StreamSpeech(context.Background(), r, func(a lebro.MediaAsset, b []byte) error {
		chunks++
		if len(b) > 32<<10 {
			t.Error("unbounded chunk")
		}
		_, _ = got.Write(b)
		time.Sleep(time.Millisecond)
		return nil
	})
	if e != nil || !bytes.Equal(got.Bytes(), payload) || chunks < 2 {
		t.Fatalf("stream %d %v", chunks, e)
	}
	stop := errors.New("consumer stopped")
	_, e = c.StreamSpeech(context.Background(), r, func(lebro.MediaAsset, []byte) error { return stop })
	if !errors.Is(e, stop) {
		t.Fatal(e)
	}
	c.caps.MaxOutputBytes = 1
	oversized, e := c.SynthesizeSpeech(context.Background(), r)
	if e == nil {
		_, e = io.ReadAll(oversized.Audio.Reader)
		_ = oversized.Audio.Reader.Close()
	}
	if e == nil {
		t.Fatal("accepted oversized output")
	}
}
func TestVideoWire(t *testing.T) {
	var posts atomic.Int32
	c := adapter(t, "openrouter", "google/veo-3.1", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			posts.Add(1)
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["duration"] != float64(8) || in["resolution"] != "1080p" {
				t.Errorf("bad video payload %v", in)
			}
			w.WriteHeader(202)
			_, _ = io.WriteString(w, `{"id":"job-1","status":"pending"}`)
		case strings.HasSuffix(r.URL.Path, "/content"):
			if r.URL.Query().Get("index") != "0" {
				t.Error("bad index")
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("video"))
		default:
			_, _ = io.WriteString(w, `{"id":"job-1","status":"completed","generation_id":"gen-1","unsigned_urls":["https://evil.invalid/?secret=1"],"usage":{"cost":0.25}}`)
		}
	})
	j, e := c.SubmitVideo(context.Background(), lebro.VideoRequest{Operation: operation(), Prompt: "video", DurationSeconds: 8, Resolution: "1080p"})
	if e != nil {
		t.Fatal(e)
	}
	j, e = c.GetVideo(context.Background(), j)
	if e != nil || j.State != lebro.MediaJobSucceeded || j.Info.Usage.CostUSD.String() != "0.25" {
		t.Fatalf("job %+v %v", j, e)
	}
	content, e := c.OpenVideo(context.Background(), j, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = content.Reader.Close() }()
	b, e := io.ReadAll(content.Reader)
	if e != nil || string(b) != "video" || posts.Load() != 1 {
		t.Fatal("bad video output")
	}
}
func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer evil.Close()
	c := adapter(t, "openai", "gpt-image-1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	})
	_, e := c.GenerateImages(context.Background(), lebro.ImageRequest{Operation: operation(), Prompt: "draw"})
	if e == nil || leaked.Load() {
		t.Fatal("followed credentialed redirect")
	}
}
func TestLiveTranscriptionReplacesProvisionalText(t *testing.T) {
	c := adapter(t, "openai", "gpt-live-transcribe", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		ws, e := up.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		var event map[string]any
		if e = ws.ReadJSON(&event); e != nil {
			return
		}
		if event["type"] != "session.update" {
			t.Error("missing session update")
		}
		_ = ws.WriteJSON(map[string]any{"type": "session.updated"})
		for {
			if e = ws.ReadJSON(&event); e != nil {
				return
			}
			if event["type"] == "input_audio_buffer.commit" {
				break
			}
		}
		for _, out := range []map[string]any{{"type": "conversation.item.input_audio_transcription.delta", "item_id": "segment-1", "delta": "hel"}, {"type": "conversation.item.input_audio_transcription.delta", "item_id": "segment-1", "delta": "p"}, {"type": "conversation.item.input_audio_transcription.completed", "item_id": "segment-1", "transcript": "hello"}} {
			_ = ws.WriteJSON(out)
		}
	})
	sent := false
	var events []lebro.TranscriptionEvent
	e := c.StreamTranscription(context.Background(), lebro.LiveTranscriptionRequest{Operation: operation()}, func(ctx context.Context, b []byte) (int, error) {
		if sent {
			return 0, io.EOF
		}
		sent = true
		return 4800, io.EOF
	}, func(ev lebro.TranscriptionEvent) error { events = append(events, ev); return nil })
	if e != nil {
		t.Fatal(e)
	}
	if len(events) != 3 || events[1].Segment.Text != "help" || events[2].Segment.Text != "hello" || !events[2].Terminal {
		t.Fatalf("bad events %+v", events)
	}
}
func TestConfigAndProfiles(t *testing.T) {
	for _, cfg := range []Config{{}, {APIKey: "key", Model: "model", BaseURL: "https://user:pass@example.com"}, {APIKey: "key", Model: "model", Timeout: -1}, {APIKey: "key", Model: "model", Capabilities: &lebro.MediaCapabilities{MaxInputBytes: -1}}} {
		if _, e := New(cfg, "openai"); e == nil {
			t.Fatal("accepted invalid config")
		}
	}
	c, e := New(Config{APIKey: "key", Model: "unknown"}, "openai")
	if e != nil {
		t.Fatal(e)
	}
	if c.Capabilities().Image {
		t.Fatal("fabricated model capabilities")
	}
	if _, e = c.GenerateImages(context.Background(), lebro.ImageRequest{Operation: operation(), Prompt: "draw"}); e == nil {
		t.Fatal("unknown image model accepted")
	}
	for _, m := range []string{"gpt-image-1", "whisper-1", "gpt-4o-mini-tts", "gpt-live-transcribe"} {
		if _, e = New(Config{APIKey: "key", Model: m}, "openai"); e != nil {
			t.Fatal(e)
		}
	}
}

func TestWebPDimensions(t *testing.T) {
	lossy := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x1a\x00\x00\x00"), make([]byte, 10)...)
	lossy[23], lossy[24], lossy[25] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(lossy[26:28], 2)
	binary.LittleEndian.PutUint16(lossy[28:30], 3)
	if w, h, ok := webpDimensions(lossy); !ok || w != 2 || h != 3 {
		t.Fatalf("lossy dims %d %d %v", w, h, ok)
	}
	lossless := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8L\x05\x00\x00\x00"), make([]byte, 6)...)
	lossless[20] = 0x2f
	binary.LittleEndian.PutUint32(lossless[21:25], 26|34<<14)
	if w, h, ok := webpDimensions(lossless); !ok || w != 27 || h != 35 {
		t.Fatalf("lossless dims %d %d %v", w, h, ok)
	}
	extended := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0d\x00\x00\x00"), make([]byte, 13)...)
	extended[27], extended[30] = 99, 64
	if w, h, ok := webpDimensions(extended); !ok || w != 100 || h != 65 {
		t.Fatalf("extended dims %d %d %v", w, h, ok)
	}
	if _, _, ok := webpDimensions([]byte("not webp data here")); ok {
		t.Fatal("accepted non-webp")
	}
	if _, _, ok := webpDimensions(lossy[:25]); ok {
		t.Fatal("accepted truncated lossy")
	}
	if _, _, ok := webpDimensions(lossless[:25]); ok {
		t.Fatal("accepted truncated lossless")
	}
	if _, _, ok := webpDimensions(extended[:32]); ok {
		t.Fatal("accepted truncated extended")
	}
}

func TestMediaResponseAndValidationEdges(t *testing.T) {
	for _, mimeType := range []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/flac", "unknown"} {
		if audioSignature([]byte("bad"), mimeType) {
			t.Fatal("accepted corrupt header")
		}
	}
	for _, mimeType := range []string{"text/plain", "audio/mpeg"} {
		c := adapter(t, "openai", "tts-1", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", mimeType) })
		result, e := c.SynthesizeSpeech(context.Background(), lebro.SpeechRequest{Operation: operation(), Text: "say", Voice: "alloy"})
		if e == nil {
			_, e = io.ReadAll(result.Audio.Reader)
			_ = result.Audio.Reader.Close()
		}
		if e == nil {
			t.Fatal("accepted empty/non-audio output")
		}
	}
	c := adapter(t, "openai", "tts-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{1, 2, 3, 4})
	})
	r := lebro.SpeechRequest{Operation: operation(), Text: "say", Voice: "alloy", Format: "pcm", Speed: 1}
	result, e := c.SynthesizeSpeech(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	if result.Audio.Asset.SampleRate != 24000 {
		t.Fatal("lost PCM metadata")
	}
	_, _ = io.Copy(io.Discard, result.Audio.Reader)
	_ = result.Audio.Reader.Close()
	for _, bad := range []lebro.SpeechRequest{{Operation: operation(), Text: "say"}, {Operation: operation(), Text: "say", Voice: "invalid"}, {Operation: operation(), Text: "say", Voice: "alloy", Format: "wav"}, {Operation: operation(), Text: "say", Voice: "alloy", Speed: -1}, {Operation: operation(), Text: "say", Voice: "alloy", Speed: math.NaN()}, {Operation: operation(), Voice: "alloy"}} {
		if _, e = c.SynthesizeSpeech(context.Background(), bad); e == nil {
			t.Fatal("accepted unsupported speech options")
		}
	}
	if _, e = c.StreamSpeech(context.Background(), r, nil); e == nil {
		t.Fatal("accepted nil consumer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = c.SynthesizeSpeech(ctx, r); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	c.caps.Speech = false
	if _, e = c.SynthesizeSpeech(context.Background(), r); e == nil {
		t.Fatal("unsupported speech accepted")
	}
	c.caps.StreamingSpeech = false
	if _, e = c.StreamSpeech(context.Background(), r, func(lebro.MediaAsset, []byte) error { return nil }); e == nil {
		t.Fatal("unsupported stream accepted")
	}
}
func TestTranscriptionFailureEdges(t *testing.T) {
	c := adapter(t, "openai", "whisper-1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{}`)
	})
	r := lebro.TranscriptionRequest{Operation: operation(), Audio: lebro.MediaContent{Asset: lebro.MediaAsset{Kind: lebro.MediaAudio, MIMEType: "audio/wav"}, Data: wavData()}}
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("accepted missing transcript")
	}
	r.Audio.Asset.Bytes = 100 << 20
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("accepted oversized audio")
	}
	r.Audio.Asset.Bytes = 0
	r.Audio.Asset.MIMEType = "audio/aac"
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("accepted unknown audio")
	}
	r.Audio.Asset.MIMEType = "audio/wav"
	r.Audio.Data = nil
	r.Audio.Asset.Locator = "key"
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("implicitly opened locator")
	}
	r.Audio.Asset.Locator = ""
	r.Audio.Reader = io.NopCloser(strings.NewReader(""))
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("empty source")
	}
	r.Language = strings.Repeat("x", 17)
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("over-limit hint")
	}
	c.caps.Transcription = false
	if _, e := c.Transcribe(context.Background(), r); e == nil {
		t.Fatal("unsupported STT accepted")
	}
}
func TestVideoValidationAndTerminalStates(t *testing.T) {
	for _, status := range []string{"pending", "queued", "in_progress", "completed", "failed", "cancelled", "expired"} {
		if _, e := videoState(status); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := videoState("unknown"); e == nil {
		t.Fatal("unknown state")
	}
	c := adapter(t, "openrouter", "google/veo-3.1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"wrong","status":"completed"}`)
	})
	for _, r := range []lebro.VideoRequest{{Operation: operation(), Prompt: "video", DurationSeconds: 100}, {Operation: operation(), Prompt: "video", Resolution: "4K"}, {Operation: operation(), Prompt: "video", Resolution: "1080p", DurationSeconds: 4}, {Operation: operation()}} {
		if _, e := c.SubmitVideo(context.Background(), r); e == nil {
			t.Fatal("unsupported video options")
		}
	}
	j := lebro.VideoJob{Info: lebro.MediaResultInfo{Operation: operation(), Provider: "openrouter", Model: c.model}, ProviderJobID: "job", State: lebro.MediaJobSucceeded, OutputCount: 1}
	if _, e := c.GetVideo(context.Background(), j); e == nil {
		t.Fatal("changed job ID")
	}
	if _, e := c.OpenVideo(context.Background(), j, 1); e == nil {
		t.Fatal("bad index")
	}
	j.Info.Provider = "different"
	if _, e := c.GetVideo(context.Background(), j); e == nil {
		t.Fatal("wrong provider")
	}
	c.caps.Video = false
	if _, e := c.SubmitVideo(context.Background(), lebro.VideoRequest{}); e == nil {
		t.Fatal("unsupported video")
	}
}
func TestLiveFailuresAndCancellation(t *testing.T) {
	for _, scenario := range []string{"unauthorized", "rejected", "disconnect", "bad-chunk", "empty", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			c := adapter(t, "openai", "gpt-live-transcribe", func(w http.ResponseWriter, r *http.Request) {
				if scenario == "unauthorized" {
					w.WriteHeader(401)
					return
				}
				ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer func() { _ = ws.Close() }()
				var in map[string]any
				if ws.ReadJSON(&in) != nil {
					return
				}
				if scenario == "rejected" {
					_ = ws.WriteJSON(map[string]any{"type": "error"})
					return
				}
				_ = ws.WriteJSON(map[string]any{"type": "session.updated"})
				if scenario == "disconnect" {
					return
				}
				for {
					if ws.ReadJSON(&in) != nil {
						return
					}
				}
			})
			deadline := 10 * time.Second
			if scenario == "cancel" {
				deadline = 50 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			e := c.StreamTranscription(ctx, lebro.LiveTranscriptionRequest{Operation: operation()}, func(ctx context.Context, b []byte) (int, error) {
				if scenario == "bad-chunk" {
					return 3, io.EOF
				}
				if scenario == "empty" {
					return 0, io.EOF
				}
				<-ctx.Done()
				return 0, ctx.Err()
			}, func(lebro.TranscriptionEvent) error { return nil })
			if e == nil {
				t.Fatal("failure accepted")
			}
			if scenario == "unauthorized" {
				var me *lebro.MediaError
				if !errors.As(e, &me) || me.Kind != lebro.MediaErrorAuthentication {
					t.Fatal(e)
				}
			}
		})
	}
}

type denyMediaPolicy struct{}

func (denyMediaPolicy) Authorize(context.Context, lebro.Identity, lebro.Action, lebro.Resource) lebro.Decision {
	return lebro.Deny("private policy reason")
}
func TestMediaAuthorizationPrecedesRequestAndTelemetry(t *testing.T) {
	var calls atomic.Int32
	c := adapter(t, "openai", "tts-1", func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	store := lebro.NewMemoryStore()
	c.attempts = store.ModelAttempts()
	c.policy = denyMediaPolicy{}
	_, e := c.SynthesizeSpeech(context.Background(), lebro.SpeechRequest{Operation: operation(), Text: "say", Voice: "alloy"})
	if e == nil || strings.Contains(e.Error(), "private") {
		t.Fatal(e)
	}
	records, e := store.ModelAttempts().ListModelAttempts(context.Background(), lebro.ModelAttemptFilter{}, lebro.PageRequest{})
	if e != nil || len(records.Records) != 0 || calls.Load() != 0 {
		t.Fatal("unauthorized side effect")
	}
	c.policy = nil
	ctx := lebro.WithRuntimeScope(context.Background(), lebro.RuntimeScope{Namespace: "verified"})
	if _, e = c.SynthesizeSpeech(ctx, lebro.SpeechRequest{Operation: operation(), Text: "say", Voice: "alloy"}); e == nil {
		t.Fatal("untrusted scope")
	}
}
func TestLiveRejectsPrematureFinalization(t *testing.T) {
	c := adapter(t, "openai", "gpt-live-transcribe", func(w http.ResponseWriter, r *http.Request) {
		ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		var in any
		if ws.ReadJSON(&in) != nil {
			return
		}
		_ = ws.WriteJSON(map[string]any{"type": "session.updated"})
		_ = ws.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "s", "transcript": "invented"})
	})
	e := c.StreamTranscription(context.Background(), lebro.LiveTranscriptionRequest{Operation: operation()}, func(ctx context.Context, b []byte) (int, error) { <-ctx.Done(); return 0, ctx.Err() }, func(lebro.TranscriptionEvent) error { t.Error("delivered premature final"); return nil })
	if e == nil {
		t.Fatal("premature success")
	}
}

func TestSpeechMIMEAndLiveOptionValidation(t *testing.T) {
	c := adapter(t, "openai", "tts-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wavData())
	})
	if _, e := c.SynthesizeSpeech(context.Background(), lebro.SpeechRequest{Operation: operation(), Text: "say", Voice: "alloy", Format: "mp3"}); e == nil {
		t.Fatal("silently mislabelled WAV as MP3")
	}
	source := func(context.Context, []byte) (int, error) { return 0, io.EOF }
	sink := func(lebro.TranscriptionEvent) error { return nil }
	if e := c.StreamTranscription(context.Background(), lebro.LiveTranscriptionRequest{Operation: operation()}, source, sink); e == nil {
		t.Fatal("non-live model accepted")
	}
	c.caps.StreamingTranscription = true
	if e := c.StreamTranscription(context.Background(), lebro.LiveTranscriptionRequest{Operation: operation()}, nil, sink); e == nil {
		t.Fatal("nil source accepted")
	}
	if e := c.StreamTranscription(context.Background(), lebro.LiveTranscriptionRequest{Operation: operation(), Prompt: strings.Repeat("x", 4097)}, source, sink); e == nil {
		t.Fatal("oversized prompt accepted")
	}
}
