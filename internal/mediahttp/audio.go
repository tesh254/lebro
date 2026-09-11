package mediahttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tesh254/lebro"
)

type boundedBody struct {
	read   int64
	body   io.ReadCloser
	left   int64
	cancel context.CancelFunc
	once   sync.Once
	onEnd  func(error) error
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.body.Read(p)
	if int64(n) > b.left {
		err = invalid("output exceeds byte limit")
		_ = b.finish(err)
		return 0, err
	}
	b.left -= int64(n)
	b.read += int64(n)
	if errors.Is(err, io.EOF) && b.read == 0 {
		err = malformed("empty media response")
	}
	if err != nil {
		outcome := err
		if errors.Is(err, io.EOF) {
			outcome = nil
		}
		if e := b.finish(outcome); e != nil {
			err = e
		}
	}
	return n, err
}
func (b *boundedBody) Close() error {
	return b.finish(context.Canceled)
}
func (b *boundedBody) finish(outcome error) error {
	var err error
	b.once.Do(func() {
		err = b.body.Close()
		b.cancel()
		if b.onEnd != nil {
			err = errors.Join(err, b.onEnd(outcome))
		}
	})
	return err
}
func (c *Client) content(resp *http.Response, cancel context.CancelFunc, kind lebro.MediaKind) (lebro.MediaContent, error) {
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, string(kind)+"/") {
		_ = resp.Body.Close()
		cancel()
		return lebro.MediaContent{}, malformed("unexpected media content type")
	}
	if resp.ContentLength > c.caps.MaxOutputBytes {
		_ = resp.Body.Close()
		cancel()
		return lebro.MediaContent{}, invalid("output exceeds byte limit")
	}
	a := lebro.MediaAsset{Kind: kind, MIMEType: mt, Provider: c.provider, Model: c.model}
	if resp.ContentLength > 0 {
		a.Bytes = resp.ContentLength
	}
	return lebro.MediaContent{Asset: a, Reader: &boundedBody{body: resp.Body, left: c.caps.MaxOutputBytes, cancel: cancel}}, nil
}
func (c *Client) Transcribe(ctx context.Context, r lebro.TranscriptionRequest) (result lebro.TranscriptionResult, retErr error) {
	if err := c.authorize(ctx, r.Operation, "media.generate"); err != nil {
		return result, err
	}
	start := time.Now().UTC()
	defer func() {
		retErr = errors.Join(retErr, c.record(ctx, result.Info, r.Operation, "transcription", start, retErr))
	}()
	if !c.caps.Transcription {
		return result, unsupported("batch transcription is unsupported")
	}
	if err := r.Operation.Validate(); err != nil {
		return result, err
	}
	if err := r.Audio.Validate(); err != nil {
		return result, err
	}
	if !slices.Contains(c.caps.InputMIMETypes, r.Audio.Asset.MIMEType) {
		return result, unsupported("unsupported audio format")
	}
	if r.Language != "" && !c.caps.Language || r.Prompt != "" && !c.caps.PromptHint || r.Timestamps && !c.caps.Timestamps {
		return result, unsupported("unsupported transcription option")
	}
	if len(r.Language) > 16 || len(r.Prompt) > 4096 {
		return result, invalid("over-limit transcription hints")
	}
	if r.Audio.Asset.Bytes > c.caps.MaxInputBytes || int64(len(r.Audio.Data)) > c.caps.MaxInputBytes {
		return result, invalid("audio exceeds byte limit")
	}
	if r.Audio.TemporaryURL != "" || r.Audio.Asset.Locator != "" {
		return result, unsupported("open audio in application storage before transcription")
	}
	src := r.Audio.Reader
	if src == nil {
		src = io.NopCloser(bytes.NewReader(r.Audio.Data))
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = src.Close() })
	defer stop()
	// Check the signature before sending billable work. Full codec validation is
	// provider-owned; Lebro neither decodes nor transcodes recordings.
	buffered := bufio.NewReader(src)
	head, err := buffered.Peek(12)
	if err != nil && len(head) == 0 {
		return result, invalid("empty audio")
	}
	if !audioSignature(head, r.Audio.Asset.MIMEType) {
		return result, invalid("audio header does not match declared format")
	}
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	done := make(chan error, 1)
	go func() {
		e := writer.WriteField("model", c.model)
		fields := map[string]string{"language": r.Language, "prompt": r.Prompt, "response_format": "json"}
		if r.Timestamps {
			fields["response_format"] = "verbose_json"
			fields["timestamp_granularities[]"] = "word"
		}
		for k, v := range fields {
			if e == nil && v != "" {
				e = writer.WriteField(k, v)
			}
		}
		if e == nil {
			var part io.Writer
			part, e = writer.CreateFormFile("file", "recording."+audioExtension(r.Audio.Asset.MIMEType))
			if e == nil {
				_, e = lebro.CopyMedia(ctx, part, buffered, c.caps.MaxInputBytes)
			}
		}
		if e == nil {
			e = writer.Close()
		}
		_ = pw.CloseWithError(e)
		done <- e
	}()
	resp, requestErr := c.request(ctx, http.MethodPost, "/audio/transcriptions", writer.FormDataContentType(), pr)
	if requestErr != nil {
		// Stop the upload on early rejection, including a source blocked in Read.
		cancel()
		_ = pr.Close()
		_ = src.Close()
	}
	uploadErr := <-done
	if requestErr != nil {
		return result, requestErr
	}
	defer func() { _ = resp.Body.Close() }()
	if uploadErr != nil {
		return result, uploadErr
	}
	result.Info = c.info(r.Operation, resp.Header)
	b, err := readBounded(resp.Body, 4<<20)
	if err != nil {
		return result, err
	}
	var wire struct {
		Text     *string                    `json:"text"`
		Language string                     `json:"language"`
		Duration *float64                   `json:"duration"`
		Segments []wireSegment              `json:"segments"`
		Words    []wireSegment              `json:"words"`
		Usage    map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(b, &wire) != nil || wire.Text == nil {
		return result, malformed("transcription missing text")
	}
	result.Info.Usage = usage(wire.Usage)
	result.Text = *wire.Text
	result.Language = wire.Language
	result.DurationSeconds = wire.Duration
	for i, s := range wire.Segments {
		result.Segments = append(result.Segments, s.segment(i))
	}
	for i, s := range wire.Words {
		result.Words = append(result.Words, s.segment(i))
	}
	return result, nil
}

type wireSegment struct {
	ID         json.RawMessage `json:"id"`
	Text       string          `json:"text"`
	Word       string          `json:"word"`
	Start      *float64        `json:"start"`
	End        *float64        `json:"end"`
	Speaker    json.RawMessage `json:"speaker"`
	Confidence *float64        `json:"confidence"`
}

func (s wireSegment) segment(i int) lebro.TranscriptSegment {
	id := strconv.Itoa(i)
	if len(s.ID) > 0 {
		id = string(s.ID)
		var str string
		if json.Unmarshal(s.ID, &str) == nil {
			id = str
		}
	}
	speaker := ""
	if len(s.Speaker) > 0 && string(s.Speaker) != "null" {
		speaker = string(s.Speaker)
		var str string
		if json.Unmarshal(s.Speaker, &str) == nil {
			speaker = str
		}
	}
	text := s.Text
	if text == "" {
		text = s.Word
	}
	return lebro.TranscriptSegment{ID: id, Text: text, StartSeconds: s.Start, EndSeconds: s.End, Speaker: speaker, Confidence: s.Confidence}
}
func audioExtension(m string) string {
	return map[string]string{"audio/wav": "wav", "audio/mpeg": "mp3", "audio/mp4": "m4a", "audio/webm": "webm", "audio/ogg": "ogg", "audio/flac": "flac"}[m]
}
func audioSignature(b []byte, m string) bool {
	switch m {
	case "audio/wav":
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE"
	case "audio/mpeg":
		return bytes.HasPrefix(b, []byte("ID3")) || len(b) >= 2 && b[0] == 255 && b[1]&224 == 224
	case "audio/mp4":
		return len(b) >= 8 && string(b[4:8]) == "ftyp"
	case "audio/webm":
		return bytes.HasPrefix(b, []byte{0x1a, 0x45, 0xdf, 0xa3})
	case "audio/ogg":
		return bytes.HasPrefix(b, []byte("OggS"))
	case "audio/flac":
		return bytes.HasPrefix(b, []byte("fLaC"))
	}
	return false
}
func (c *Client) SynthesizeSpeech(ctx context.Context, r lebro.SpeechRequest) (result lebro.SpeechResult, retErr error) {
	if err := c.authorize(ctx, r.Operation, "media.generate"); err != nil {
		return result, err
	}
	start := time.Now().UTC()
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, c.record(ctx, result.Info, r.Operation, "speech", start, retErr))
		}
	}()
	if !c.caps.Speech {
		return result, unsupported("speech synthesis is unsupported")
	}
	if err := c.text(r.Operation, r.Text); err != nil {
		return result, err
	}
	if r.Format == "" {
		r.Format = "mp3"
	}
	if err := allowed(r.Format, c.caps.OutputFormats); err != nil {
		return result, err
	}
	if r.Voice == "" {
		return result, invalid("explicit provider voice is required")
	}
	if err := allowed(r.Voice, c.caps.Voices); err != nil {
		return result, err
	}
	if math.IsNaN(r.Speed) || math.IsInf(r.Speed, 0) || r.Speed != 0 && (!c.caps.Speed || r.Speed < 0.25 || r.Speed > 4) {
		return result, unsupported("unsupported speech speed")
	}
	payload := map[string]any{"model": c.model, "input": r.Text, "voice": r.Voice, "response_format": r.Format}
	if r.Speed != 0 {
		payload["speed"] = r.Speed
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	resp, err := c.request(ctx, http.MethodPost, "/audio/speech", "application/json", bytes.NewReader(body))
	if err != nil {
		cancel()
		return result, err
	}
	// PCM may use application/octet-stream; format was explicitly requested.
	if r.Format == "pcm" && resp.Header.Get("Content-Type") == "application/octet-stream" {
		resp.Header.Set("Content-Type", "audio/pcm")
	}
	result.Info = c.info(r.Operation, resp.Header)
	result.Audio, err = c.content(resp, cancel, lebro.MediaAudio)
	if err == nil {
		expected := map[string]string{"mp3": "audio/mpeg", "pcm": "audio/pcm"}[r.Format]
		if expected != "" && result.Audio.Asset.MIMEType != expected {
			_ = result.Audio.Reader.Close()
			return result, malformed("speech response does not match requested format")
		}
		result.Audio.Reader.(*boundedBody).onEnd = func(outcome error) error { return c.record(ctx, result.Info, r.Operation, "speech", start, outcome) }
		result.Audio.Asset.Codec = r.Format
		result.Audio.Asset.ProviderRequestID = result.Info.ProviderRequestID
		if r.Format == "pcm" && c.provider == "openai" {
			result.Audio.Asset.SampleRate = 24000
			result.Audio.Asset.Channels = 1
		}
	}
	return result, err
}
func (c *Client) StreamSpeech(ctx context.Context, r lebro.SpeechRequest, sink func(lebro.MediaAsset, []byte) error) (lebro.MediaResultInfo, error) {
	if !c.caps.StreamingSpeech {
		return lebro.MediaResultInfo{}, unsupported("streaming speech is unsupported")
	}
	if sink == nil {
		return lebro.MediaResultInfo{}, invalid("audio consumer is required")
	}
	result, err := c.SynthesizeSpeech(ctx, r)
	if err != nil {
		return result.Info, err
	}
	defer func() { _ = result.Audio.Reader.Close() }()
	buf := make([]byte, 32<<10)
	for {
		if err = ctx.Err(); err != nil {
			return result.Info, err
		}
		n, e := result.Audio.Reader.Read(buf)
		if n > 0 {
			if err = sink(result.Audio.Asset, buf[:n]); err != nil {
				return result.Info, err
			}
		}
		if errors.Is(e, io.EOF) {
			return result.Info, nil
		}
		if e != nil {
			return result.Info, e
		}
	}
}
