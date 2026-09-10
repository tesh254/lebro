package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// MediaKind identifies a generated artifact, not a chat message part.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
	MediaAudio MediaKind = "audio"
)

// MediaOperation supplies an application-generated identity. Reuse the same ID
// when resuming a job, never for a different prompt. Scope is application trusted.
type MediaOperation struct {
	ID         string       `json:"id"`
	Scope      RuntimeScope `json:"scope"`
	RunID      RunID        `json:"run_id,omitempty"`
	ThreadID   ThreadID     `json:"thread_id,omitempty"`
	WorkflowID WorkflowID   `json:"workflow_id,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

func (o MediaOperation) Validate() error {
	for _, s := range []string{o.ID, o.Scope.Namespace, o.Scope.OwnerID, string(o.RunID), string(o.ThreadID), string(o.WorkflowID), o.ToolCallID} {
		if len(s) > 256 || strings.ContainsAny(s, "\r\n\x00") {
			return mediaInvalid("invalid operation identity")
		}
	}
	if strings.TrimSpace(o.ID) == "" {
		return mediaInvalid("operation ID is required")
	}
	return nil
}

// MediaAsset is serializable metadata. Locator is an application-owned opaque
// storage key, never a temporary provider URL. Zero optional measurements mean
// unknown; a pointer to zero duration or confidence means a reported zero.
type MediaAsset struct {
	ID                string     `json:"id,omitempty"`
	Locator           string     `json:"locator,omitempty"`
	Kind              MediaKind  `json:"kind"`
	MIMEType          string     `json:"mime_type"`
	Filename          string     `json:"filename,omitempty"`
	Bytes             int64      `json:"bytes,omitempty"`
	SHA256            string     `json:"sha256,omitempty"`
	Width             int        `json:"width,omitempty"`
	Height            int        `json:"height,omitempty"`
	DurationSeconds   *float64   `json:"duration_seconds,omitempty"`
	Codec             string     `json:"codec,omitempty"`
	SampleRate        int        `json:"sample_rate,omitempty"`
	Channels          int        `json:"channels,omitempty"`
	Provider          string     `json:"provider,omitempty"`
	Model             string     `json:"model,omitempty"`
	ProviderRequestID string     `json:"provider_request_id,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

// MediaContent contains exactly one content source. It is deliberately not
// JSON serializable: persist Asset only after copying to application storage.
// A returned Reader belongs to the caller, who must Close it even on failure.
// An input Reader belongs to the caller and must unblock when closed.
type MediaContent struct {
	Asset        MediaAsset
	Data         []byte
	Reader       io.ReadCloser
	TemporaryURL string
}

func (MediaContent) MarshalJSON() ([]byte, error) {
	return nil, mediaInvalid("persist an asset descriptor, not media content")
}
func (c MediaContent) Validate() error {
	n := 0
	if c.Data != nil {
		n++
	}
	if c.Reader != nil {
		n++
	}
	if c.TemporaryURL != "" {
		n++
	}
	if c.Asset.Locator != "" {
		n++
	}
	if n != 1 {
		return mediaInvalid("exactly one media source is required")
	}
	if c.Data != nil && len(c.Data) == 0 {
		return mediaInvalid("media is empty")
	}
	if c.Asset.Kind != MediaImage && c.Asset.Kind != MediaVideo && c.Asset.Kind != MediaAudio {
		return mediaInvalid("invalid media kind")
	}
	if !strings.HasPrefix(c.Asset.MIMEType, string(c.Asset.Kind)+"/") {
		return mediaInvalid("MIME type does not match media kind")
	}
	if c.Asset.Bytes < 0 || c.Asset.Width < 0 || c.Asset.Height < 0 {
		return mediaInvalid("negative media measurement")
	}
	return nil
}

// MediaErrorKind is shared across media adapters. Error text contains only safe
// SDK diagnostics. Cause is retained for errors.Is, never formatted by Error.
type MediaErrorKind string

const (
	MediaErrorValidation     MediaErrorKind = "validation"
	MediaErrorUnsupported    MediaErrorKind = "unsupported"
	MediaErrorAuthentication MediaErrorKind = "authentication"
	MediaErrorAuthorization  MediaErrorKind = "authorization"
	MediaErrorRateLimit      MediaErrorKind = "rate_limit"
	MediaErrorQuota          MediaErrorKind = "quota"
	MediaErrorRefusal        MediaErrorKind = "refusal"
	MediaErrorTimeout        MediaErrorKind = "timeout"
	MediaErrorCancelled      MediaErrorKind = "cancelled"
	MediaErrorTransport      MediaErrorKind = "transport"
	MediaErrorMalformed      MediaErrorKind = "malformed_response"
	MediaErrorRemote         MediaErrorKind = "remote_failure"
	MediaErrorAmbiguous      MediaErrorKind = "ambiguous_submission"
	MediaErrorExpired        MediaErrorKind = "expired"
)

type MediaError struct {
	Kind         MediaErrorKind
	Message      string
	ProviderCode string
	RetryAfter   time.Duration
	Retryable    bool
	Cause        error
}

func (e *MediaError) Error() string { return "lebro media: " + string(e.Kind) + ": " + e.Message }
func (e *MediaError) Unwrap() error { return e.Cause }
func mediaInvalid(s string) error   { return &MediaError{Kind: MediaErrorValidation, Message: s} }

// MediaUsage preserves native units and exact decimal cost when reported.
// Missing map entries and nil CostUSD are unknown, not zero. Cost is not estimated.
type MediaUsage struct {
	Units   map[string]json.Number `json:"units,omitempty"`
	CostUSD *json.Number           `json:"cost_usd,omitempty"`
}
type MediaResultInfo struct {
	Operation         MediaOperation `json:"operation"`
	Provider          string         `json:"provider"`
	Model             string         `json:"model"`
	ProviderRequestID string         `json:"provider_request_id,omitempty"`
	Usage             MediaUsage     `json:"usage,omitempty"`
}

// MediaCapabilities describes what this configured adapter implements. Empty
// option lists mean unsupported/unknown, never unrestricted support.
type MediaCapabilities struct {
	Provider               string
	Model                  string
	Image                  bool
	Video                  bool
	Transcription          bool
	Speech                 bool
	StreamingTranscription bool
	StreamingSpeech        bool
	RemoteCancel           bool
	InputMIMETypes         []string
	OutputFormats          []string
	Sizes                  []string
	Resolutions            []string
	AspectRatios           []string
	Durations              []int
	Qualities              []string
	Voices                 []string
	MaxCount               int
	MaxInputBytes          int64
	MaxOutputBytes         int64
	MaxTextBytes           int
	Timestamps             bool
	Language               bool
	PromptHint             bool
	Speed                  bool
}
type MediaCapabilityProvider interface{ Capabilities() MediaCapabilities }
type ImageRequest struct {
	Operation   MediaOperation
	Prompt      string
	Count       int
	Size        string
	Resolution  string
	AspectRatio string
	Quality     string
	Format      string
}
type ImageResult struct {
	Info           MediaResultInfo
	Images         []MediaContent
	RevisedPrompts []string
	Partial        bool
}
type ImageGenerator interface {
	GenerateImages(context.Context, ImageRequest) (ImageResult, error)
}
type VideoRequest struct {
	Operation       MediaOperation
	Prompt          string
	DurationSeconds int
	Size            string
	Resolution      string
	AspectRatio     string
}
type MediaJobState string

const (
	MediaJobSubmitting MediaJobState = "submitting"
	MediaJobAmbiguous  MediaJobState = "ambiguous"
	MediaJobQueued     MediaJobState = "queued"
	MediaJobRunning    MediaJobState = "running"
	MediaJobSucceeded  MediaJobState = "succeeded"
	MediaJobFailed     MediaJobState = "failed"
	MediaJobCancelled  MediaJobState = "cancelled"
	MediaJobExpired    MediaJobState = "expired"
)

func (s MediaJobState) Terminal() bool {
	return s == MediaJobSucceeded || s == MediaJobFailed || s == MediaJobCancelled || s == MediaJobExpired
}

type VideoJob struct {
	RequestHash    string          `json:"request_hash,omitempty"`
	Info           MediaResultInfo `json:"info"`
	ProviderJobID  string          `json:"provider_job_id,omitempty"`
	State          MediaJobState   `json:"state"`
	ProviderStatus string          `json:"provider_status,omitempty"`
	OutputCount    int             `json:"output_count,omitempty"`
	Assets         []MediaAsset    `json:"assets,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Revision       int64           `json:"revision"`
}
type VideoGenerator interface {
	SubmitVideo(context.Context, VideoRequest) (VideoJob, error)
	GetVideo(context.Context, VideoJob) (VideoJob, error)
	OpenVideo(context.Context, VideoJob, int) (MediaContent, error)
}

// VideoCanceller means confirmed remote cancellation is supported. A local
// context cancellation never calls it automatically.
type VideoCanceller interface {
	CancelVideo(context.Context, VideoJob) (VideoJob, error)
}

type TranscriptionRequest struct {
	Operation  MediaOperation
	Audio      MediaContent
	Language   string
	Prompt     string
	Timestamps bool
}

// TranscriptSegment times are seconds from the start of this recording.
// Speaker labels are scoped to the recording and never identify a real person.
type TranscriptSegment struct {
	ID           string   `json:"id"`
	Text         string   `json:"text"`
	StartSeconds *float64 `json:"start_seconds,omitempty"`
	EndSeconds   *float64 `json:"end_seconds,omitempty"`
	Speaker      string   `json:"speaker,omitempty"`
	Confidence   *float64 `json:"confidence,omitempty"`
}
type TranscriptionResult struct {
	Info            MediaResultInfo     `json:"info"`
	Text            string              `json:"text"`
	Language        string              `json:"language,omitempty"`
	DurationSeconds *float64            `json:"duration_seconds,omitempty"`
	Segments        []TranscriptSegment `json:"segments,omitempty"`
	Words           []TranscriptSegment `json:"words,omitempty"`
}
type Transcriber interface {
	Transcribe(context.Context, TranscriptionRequest) (TranscriptionResult, error)
}

// TranscriptionEvent replaces the entire hypothesis for Segment.ID. Final
// means that segment is settled. Terminal ends the utterance. Errors are returned
// by StreamTranscription, never encoded as successful terminal events.
type TranscriptionEvent struct {
	Segment  TranscriptSegment
	Final    bool
	Terminal bool
}
type LiveTranscriptionRequest struct {
	Operation MediaOperation
	Language  string
	Prompt    string
}

// AudioSource returns up to 32 KiB per call. io.EOF flushes one utterance.
// It must honor context cancellation and not retain the supplied buffer.
type AudioSource func(context.Context, []byte) (int, error)
type StreamingTranscriber interface {
	StreamTranscription(context.Context, LiveTranscriptionRequest, AudioSource, func(TranscriptionEvent) error) error
}
type SpeechRequest struct {
	Operation MediaOperation
	Text      string
	Voice     string
	Format    string
	Speed     float64
}
type SpeechResult struct {
	Info  MediaResultInfo
	Audio MediaContent
}
type SpeechSynthesizer interface {
	SynthesizeSpeech(context.Context, SpeechRequest) (SpeechResult, error)
}

// StreamingSpeechSynthesizer delivers consecutive bytes of ONE encoded stream,
// not separately playable container files. The callback is synchronous and must
// honor cancellation. Returning an error stops production and closes resources.
type StreamingSpeechSynthesizer interface {
	StreamSpeech(context.Context, SpeechRequest, func(MediaAsset, []byte) error) (MediaResultInfo, error)
}

// MediaAssetSink writes atomically to application-owned storage. On error it
// must remove partial content. It must consume to EOF before publishing a locator.
type MediaAssetSink interface {
	PutMedia(context.Context, MediaOperation, MediaAsset, io.Reader) (MediaAsset, error)
}

// MediaAssetOpener resolves a locator/temporary URL under application access and
// egress policy. The SDK never follows an arbitrary URL without this hook.
type MediaAssetOpener func(context.Context, MediaOperation, MediaContent) (io.ReadCloser, error)

// CopyMedia copies in bounded chunks and checks cancellation between reads.
// A blocking reader/writer must itself support cancellation; no goroutine is
// leaked to simulate interrupting arbitrary application I/O.
func CopyMedia(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, mediaInvalid("positive byte limit is required")
	}
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		size := int64(len(buf))
		if size > limit-total+1 {
			size = limit - total + 1
		}
		n, err := src.Read(buf[:size])
		if n < 0 || n > int(size) {
			return total, mediaInvalid("invalid reader count")
		}
		if int64(n) > limit-total {
			return total, mediaInvalid("media exceeds byte limit")
		}
		if n > 0 {
			written, e := dst.Write(buf[:n])
			total += int64(written)
			if e != nil {
				return total, e
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}
func validateMediaText(s string, max int) error {
	if strings.TrimSpace(s) == "" || len(s) > max {
		return mediaInvalid(fmt.Sprintf("text must contain 1..%d bytes", max))
	}
	return nil
}
