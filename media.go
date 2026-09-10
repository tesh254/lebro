// Media contracts are independent of the chat Model interface.
package lebro

import (
	"context"
	"github.com/tesh254/lebro/internal/runtime"
	"io"
	"time"
)

type (
	MediaKind                  = runtime.MediaKind
	MediaOperation             = runtime.MediaOperation
	MediaAsset                 = runtime.MediaAsset
	MediaContent               = runtime.MediaContent
	MediaErrorKind             = runtime.MediaErrorKind
	MediaError                 = runtime.MediaError
	MediaUsage                 = runtime.MediaUsage
	MediaResultInfo            = runtime.MediaResultInfo
	MediaCapabilities          = runtime.MediaCapabilities
	MediaCapabilityProvider    = runtime.MediaCapabilityProvider
	ImageRequest               = runtime.ImageRequest
	ImageResult                = runtime.ImageResult
	ImageGenerator             = runtime.ImageGenerator
	VideoRequest               = runtime.VideoRequest
	MediaJobState              = runtime.MediaJobState
	VideoJob                   = runtime.VideoJob
	VideoGenerator             = runtime.VideoGenerator
	VideoCanceller             = runtime.VideoCanceller
	TranscriptionRequest       = runtime.TranscriptionRequest
	TranscriptSegment          = runtime.TranscriptSegment
	TranscriptionResult        = runtime.TranscriptionResult
	Transcriber                = runtime.Transcriber
	TranscriptionEvent         = runtime.TranscriptionEvent
	LiveTranscriptionRequest   = runtime.LiveTranscriptionRequest
	AudioSource                = runtime.AudioSource
	StreamingTranscriber       = runtime.StreamingTranscriber
	SpeechRequest              = runtime.SpeechRequest
	SpeechResult               = runtime.SpeechResult
	SpeechSynthesizer          = runtime.SpeechSynthesizer
	StreamingSpeechSynthesizer = runtime.StreamingSpeechSynthesizer
	MediaAssetSink             = runtime.MediaAssetSink
	MediaAssetOpener           = runtime.MediaAssetOpener
	MediaJobRepository         = runtime.MediaJobRepository
	MediaJobStore              = runtime.MediaJobStore
	VideoServiceConfig         = runtime.VideoServiceConfig
	VideoService               = runtime.VideoService
	MediaWaitOptions           = runtime.MediaWaitOptions
)

const (
	MediaImage               = runtime.MediaImage
	MediaVideo               = runtime.MediaVideo
	MediaAudio               = runtime.MediaAudio
	MediaErrorValidation     = runtime.MediaErrorValidation
	MediaErrorUnsupported    = runtime.MediaErrorUnsupported
	MediaErrorAuthentication = runtime.MediaErrorAuthentication
	MediaErrorAuthorization  = runtime.MediaErrorAuthorization
	MediaErrorRateLimit      = runtime.MediaErrorRateLimit
	MediaErrorQuota          = runtime.MediaErrorQuota
	MediaErrorRefusal        = runtime.MediaErrorRefusal
	MediaErrorTimeout        = runtime.MediaErrorTimeout
	MediaErrorCancelled      = runtime.MediaErrorCancelled
	MediaErrorTransport      = runtime.MediaErrorTransport
	MediaErrorMalformed      = runtime.MediaErrorMalformed
	MediaErrorRemote         = runtime.MediaErrorRemote
	MediaErrorAmbiguous      = runtime.MediaErrorAmbiguous
	MediaErrorExpired        = runtime.MediaErrorExpired
	MediaJobSubmitting       = runtime.MediaJobSubmitting
	MediaJobContacting       = runtime.MediaJobContacting
	MediaJobAmbiguous        = runtime.MediaJobAmbiguous
	MediaJobQueued           = runtime.MediaJobQueued
	MediaJobRunning          = runtime.MediaJobRunning
	MediaJobSucceeded        = runtime.MediaJobSucceeded
	MediaJobFailed           = runtime.MediaJobFailed
	MediaJobCancelled        = runtime.MediaJobCancelled
	MediaJobExpired          = runtime.MediaJobExpired
)

func CopyMedia(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	return runtime.CopyMedia(ctx, dst, src, limit)
}
func NewVideoService(c VideoServiceConfig) (*VideoService, error) { return runtime.NewVideoService(c) }

type MediaToolOperation = runtime.MediaToolOperation
type ImageOptions = runtime.ImageOptions
type ImageToolConfig = runtime.ImageToolConfig
type VideoToolConfig = runtime.VideoToolConfig
type MediaArtifactResult = runtime.MediaArtifactResult

func NewImageTool(c ImageToolConfig) (Tool, error) { return runtime.NewImageTool(c) }
func NewVideoTool(c VideoToolConfig) (Tool, error) { return runtime.NewVideoTool(c) }
func SaveMediaAsset(ctx context.Context, o MediaOperation, c MediaContent, sink MediaAssetSink, open MediaAssetOpener, max int64) (MediaAsset, error) {
	return runtime.SaveMediaAsset(ctx, o, c, sink, open, max)
}
func MediaOperationFromToolContext(ctx context.Context) (MediaOperation, error) {
	return runtime.MediaOperationFromToolContext(ctx)
}

func RecordMediaAttempt(ctx context.Context, repo ModelAttemptRepository, info MediaResultInfo, kind string, started, finished time.Time, outcome error) error {
	return runtime.RecordMediaAttempt(ctx, repo, info, kind, started, finished, outcome)
}
