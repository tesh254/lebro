package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// MediaJobRepository is optional and independent of Store/RuntimeStore.
// SaveVideoJob atomically inserts at expectedRevision=0 or replaces the matching
// revision. Terminal records are immutable. Conflicts return ErrConflict.
type MediaJobRepository interface {
	GetVideoJob(context.Context, RuntimeScope, string) (VideoJob, error)
	SaveVideoJob(context.Context, VideoJob, int64) error
}

// MediaJobStore is an optional extension implemented by the built-in stores.
type MediaJobStore interface{ MediaJobs() MediaJobRepository }

type VideoServiceConfig struct {
	Generator VideoGenerator
	Jobs      MediaJobRepository
	Policy    Policy
	Attempts  ModelAttemptRepository
}
type VideoService struct{ config VideoServiceConfig }

func NewVideoService(c VideoServiceConfig) (*VideoService, error) {
	if c.Generator == nil || isNilInterface(c.Generator) || c.Jobs == nil || isNilInterface(c.Jobs) {
		return nil, mediaInvalid("video generator and job repository are required")
	}
	return &VideoService{config: c}, nil
}
func (s *VideoService) check(ctx context.Context, o MediaOperation, action Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if scope, ok := RuntimeScopeFromContext(ctx); ok && scope != o.Scope {
		return &MediaError{Kind: MediaErrorAuthorization, Message: "media scope mismatch"}
	}
	return authorize(ctx, s.config.Policy, action, Resource{Kind: "media_job", ID: o.ID, Tenant: o.Scope.Namespace, OwnerID: o.Scope.OwnerID})
}

// Submit reserves identity before billable work. A replay returns the existing
// handle; submitting/ambiguous records require explicit application reconciliation.
func (s *VideoService) Submit(ctx context.Context, r VideoRequest) (VideoJob, error) {
	if err := s.check(ctx, r.Operation, "media.generate"); err != nil {
		return VideoJob{}, err
	}
	body, _ := json.Marshal(r)
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	old, err := s.config.Jobs.GetVideoJob(ctx, r.Operation.Scope, r.Operation.ID)
	if err == nil {
		return sameVideoRequest(old, hash)
	}
	if !errors.Is(err, ErrNotFound) {
		return VideoJob{}, err
	}
	now := time.Now().UTC()
	job := VideoJob{Info: MediaResultInfo{Operation: r.Operation}, State: MediaJobSubmitting, CreatedAt: now, UpdatedAt: now, Revision: 1, RequestHash: hash}
	if err = s.config.Jobs.SaveVideoJob(ctx, job, 0); err != nil {
		if errors.Is(err, ErrConflict) {
			old, e := s.config.Jobs.GetVideoJob(ctx, r.Operation.Scope, r.Operation.ID)
			if e != nil {
				return VideoJob{}, e
			}
			return sameVideoRequest(old, hash)
		}
		return VideoJob{}, err
	}
	received, submitErr := s.config.Generator.SubmitVideo(ctx, r)
	if submitErr != nil {
		job.State = MediaJobAmbiguous
		var me *MediaError
		if errors.As(submitErr, &me) && (me.Kind == MediaErrorValidation || me.Kind == MediaErrorUnsupported || me.Kind == MediaErrorAuthentication || me.Kind == MediaErrorAuthorization || me.Kind == MediaErrorRefusal || me.Kind == MediaErrorQuota || me.Kind == MediaErrorRateLimit) {
			job.State = MediaJobFailed
		}
	} else {
		job.Info = received.Info
		job.Info.Operation = r.Operation
		job.ProviderJobID = received.ProviderJobID
		job.State = received.State
		job.ProviderStatus = received.ProviderStatus
		job.OutputCount = received.OutputCount
		if job.ProviderJobID == "" {
			job.State = MediaJobAmbiguous
			submitErr = &MediaError{Kind: MediaErrorAmbiguous, Message: "submission returned no job identity"}
		}
	}
	job.Revision = 2
	job.UpdatedAt = time.Now().UTC()
	// Saving accepted identity must survive local request cancellation, bounded by
	// its own deadline. A failed save still returns the handle for reconciliation.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = s.config.Jobs.SaveVideoJob(saveCtx, job, 1); err != nil {
		return job, &MediaError{Kind: MediaErrorAmbiguous, Message: "submission outcome could not be persisted; retain returned handle", Cause: err}
	}
	if job.State.Terminal() {
		if recordErr := s.record(saveCtx, job); recordErr != nil {
			return job, recordErr
		}
	}
	return job, submitErr
}
func sameVideoRequest(job VideoJob, hash string) (VideoJob, error) {
	if job.RequestHash != hash {
		return job, ErrConflict
	}
	if job.State == MediaJobSubmitting || job.State == MediaJobAmbiguous {
		return job, &MediaError{Kind: MediaErrorAmbiguous, Message: "operation requires reconciliation; do not resubmit"}
	}
	return job, nil
}
func (s *VideoService) Get(ctx context.Context, o MediaOperation) (VideoJob, error) {
	if err := s.check(ctx, o, "media.read"); err != nil {
		return VideoJob{}, err
	}
	job, err := s.config.Jobs.GetVideoJob(ctx, o.Scope, o.ID)
	if err != nil {
		return job, err
	}
	if job.State.Terminal() {
		return job, s.record(ctx, job)
	}
	if job.ProviderJobID == "" {
		return job, &MediaError{Kind: MediaErrorAmbiguous, Message: "operation has no confirmed provider job ID"}
	}
	updated, err := s.config.Generator.GetVideo(ctx, job)
	if err != nil {
		return job, err
	}
	if updated.ProviderJobID != job.ProviderJobID || updated.Info.Provider != job.Info.Provider || updated.Info.Model != job.Info.Model {
		return job, &MediaError{Kind: MediaErrorMalformed, Message: "provider changed job identity"}
	}
	// Queued observations must not regress a previously running job.
	if job.State == MediaJobRunning && updated.State == MediaJobQueued {
		return job, nil
	}
	updated.Info.Operation = job.Info.Operation
	updated.CreatedAt = job.CreatedAt
	updated.RequestHash = job.RequestHash
	updated.Revision = job.Revision + 1
	updated.UpdatedAt = time.Now().UTC()
	if err = s.config.Jobs.SaveVideoJob(ctx, updated, job.Revision); err != nil {
		if errors.Is(err, ErrConflict) {
			return s.config.Jobs.GetVideoJob(ctx, o.Scope, o.ID)
		}
		return job, err
	}
	if updated.State.Terminal() {
		err = s.record(ctx, updated)
	}
	return updated, err
}
func (s *VideoService) record(ctx context.Context, j VideoJob) error {
	var outcome error
	if j.State == MediaJobCancelled {
		outcome = context.Canceled
	} else if j.State != MediaJobSucceeded {
		outcome = &MediaError{Kind: MediaErrorRemote, Message: "video did not succeed"}
	}
	info := j.Info
	if info.ProviderRequestID == "" {
		info.ProviderRequestID = j.ProviderJobID
	}
	return RecordMediaAttempt(ctx, s.config.Attempts, info, "video", j.CreatedAt, j.UpdatedAt, outcome)
}

type MediaWaitOptions struct {
	Interval    time.Duration
	MaxInterval time.Duration
	Timeout     time.Duration
	RetryBudget int
	Jitter      float64
}

// Wait retries only status reads. Cancellation leaves the remote job untouched.
func (s *VideoService) Wait(ctx context.Context, o MediaOperation, opt MediaWaitOptions) (VideoJob, error) {
	if opt.Interval < 0 || opt.MaxInterval < 0 || opt.Timeout < 0 || opt.RetryBudget < 0 || opt.Jitter < 0 || opt.Jitter > 1 || math.IsNaN(opt.Jitter) {
		return VideoJob{}, mediaInvalid("invalid wait options")
	}
	if opt.Interval == 0 {
		opt.Interval = time.Second
	}
	if opt.MaxInterval == 0 {
		opt.MaxInterval = 10 * time.Second
	}
	if opt.MaxInterval < opt.Interval {
		return VideoJob{}, mediaInvalid("maximum interval is below interval")
	}
	if opt.Timeout == 0 {
		opt.Timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	delay := opt.Interval
	var last VideoJob
	for {
		job, err := s.Get(ctx, o)
		if job.Info.Operation.ID != "" {
			last = job
		}
		wait := delay
		if err != nil {
			var me *MediaError
			if !errors.As(err, &me) || !me.Retryable || opt.RetryBudget == 0 {
				return last, err
			}
			opt.RetryBudget--
			if me.RetryAfter > wait {
				wait = me.RetryAfter
			}
		}
		if err == nil && job.State.Terminal() {
			if job.State != MediaJobSucceeded {
				switch job.State {
				case MediaJobCancelled:
					return job, &MediaError{Kind: MediaErrorCancelled, Message: "video was cancelled"}
				case MediaJobExpired:
					return job, &MediaError{Kind: MediaErrorExpired, Message: "video expired"}
				}
				return job, &MediaError{Kind: MediaErrorRemote, Message: "video ended in state " + string(job.State)}
			}
			return job, nil
		}
		wait += time.Duration(float64(wait) * opt.Jitter * rand.Float64())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, ctx.Err()
		case <-timer.C:
		}
		if delay < opt.MaxInterval/2 {
			delay *= 2
		} else {
			delay = opt.MaxInterval
		}
	}
}
func (s *VideoService) Open(ctx context.Context, o MediaOperation, index int) (MediaContent, error) {
	if err := s.check(ctx, o, "media.read"); err != nil {
		return MediaContent{}, err
	}
	j, err := s.config.Jobs.GetVideoJob(ctx, o.Scope, o.ID)
	if err != nil {
		return MediaContent{}, err
	}
	if j.State != MediaJobSucceeded {
		return MediaContent{}, mediaInvalid("video is not complete")
	}
	return s.config.Generator.OpenVideo(ctx, j, index)
}
func (s *VideoService) Cancel(ctx context.Context, o MediaOperation) (VideoJob, error) {
	if err := s.check(ctx, o, "media.cancel"); err != nil {
		return VideoJob{}, err
	}
	c, ok := s.config.Generator.(VideoCanceller)
	if !ok {
		return VideoJob{}, &MediaError{Kind: MediaErrorUnsupported, Message: "remote cancellation is unsupported; local waits can be cancelled"}
	}
	j, err := s.config.Jobs.GetVideoJob(ctx, o.Scope, o.ID)
	if err != nil || j.State.Terminal() {
		return j, err
	}
	if j.ProviderJobID == "" || j.State == MediaJobSubmitting || j.State == MediaJobAmbiguous {
		return j, &MediaError{Kind: MediaErrorAmbiguous, Message: "operation has no confirmed provider job ID"}
	}
	next, err := c.CancelVideo(ctx, j)
	if err != nil {
		return j, err
	}
	if next.ProviderJobID != j.ProviderJobID || next.Info.Provider != j.Info.Provider || next.Info.Model != j.Info.Model {
		return j, &MediaError{Kind: MediaErrorMalformed, Message: "provider changed job identity during cancellation"}
	}
	if next.State != MediaJobCancelled && next.State != MediaJobSucceeded {
		return j, &MediaError{Kind: MediaErrorMalformed, Message: "remote cancellation was not confirmed"}
	}
	next.Info.Operation = j.Info.Operation
	next.RequestHash = j.RequestHash
	next.CreatedAt = j.CreatedAt
	next.UpdatedAt = time.Now().UTC()
	next.Revision = j.Revision + 1
	if err = s.config.Jobs.SaveVideoJob(ctx, next, j.Revision); errors.Is(err, ErrConflict) {
		return s.config.Jobs.GetVideoJob(ctx, o.Scope, o.ID)
	}
	return next, err
}
