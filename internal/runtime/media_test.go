package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mediaOp() MediaOperation {
	return MediaOperation{ID: "operation", Scope: RuntimeScope{Namespace: "tenant", OwnerID: "owner"}}
}
func TestMediaRepositoryContract(t *testing.T) {
	factories := map[string]func(*testing.T) MediaJobRepository{
		"memory": func(t *testing.T) MediaJobRepository { return NewMemoryStore().MediaJobs() },
		"sqlite": func(t *testing.T) MediaJobRepository {
			s, e := NewSQLiteStore(filepath.Join(t.TempDir(), "media.db"))
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = s.Close() })
			if e = s.Migrate(context.Background()); e != nil {
				t.Fatal(e)
			}
			return s.MediaJobs()
		},
		"postgres": func(t *testing.T) MediaJobRepository {
			dsn := os.Getenv("LEBRO_POSTGRES_TEST_DSN")
			if dsn == "" {
				t.Skip("set LEBRO_POSTGRES_TEST_DSN to run PostgreSQL media contracts")
			}
			s, e := NewPostgresStore(dsn, PostgresStoreOptions{Schema: "lebro_media_contract"})
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _, _ = s.db.Exec("DROP SCHEMA lebro_media_contract CASCADE"); _ = s.Close() })
			if e = s.Migrate(context.Background()); e != nil {
				t.Fatal(e)
			}
			return s.MediaJobs()
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			repo := factory(t)
			ctx := context.Background()
			o := mediaOp()
			now := time.Now().UTC()
			j := VideoJob{Info: MediaResultInfo{Operation: o, Provider: "fixture", Model: "video"}, State: MediaJobSubmitting, Revision: 1, CreatedAt: now, UpdatedAt: now}
			if _, e := repo.GetVideoJob(ctx, o.Scope, o.ID); !errors.Is(e, ErrNotFound) {
				t.Fatal(e)
			}
			if e := repo.SaveVideoJob(ctx, j, 0); e != nil {
				t.Fatal(e)
			}
			if e := repo.SaveVideoJob(ctx, j, 0); !errors.Is(e, ErrConflict) {
				t.Fatalf("duplicate insert %v", e)
			}
			copy, e := repo.GetVideoJob(ctx, o.Scope, o.ID)
			if e != nil || copy.Revision != 1 {
				t.Fatalf("roundtrip %+v %v", copy, e)
			}
			if _, e = repo.GetVideoJob(ctx, RuntimeScope{Namespace: "other"}, o.ID); !errors.Is(e, ErrNotFound) {
				t.Fatal("cross tenant leak")
			}
			scoped := WithRuntimeScope(ctx, RuntimeScope{Namespace: "other"})
			if _, e = repo.GetVideoJob(scoped, o.Scope, o.ID); e == nil {
				t.Fatal("trusted scope bypass")
			}
			j.State = MediaJobQueued
			j.ProviderJobID = "provider-1"
			j.Revision = 2
			if e = repo.SaveVideoJob(ctx, j, 1); e != nil {
				t.Fatal(e)
			}
			if e = repo.SaveVideoJob(ctx, j, 1); !errors.Is(e, ErrConflict) {
				t.Fatal("stale CAS accepted")
			}
			j.State = MediaJobSucceeded
			j.Revision = 3
			cost := json.Number("0")
			j.Info.Usage = MediaUsage{CostUSD: &cost, Units: map[string]json.Number{"seconds": "8"}}
			j.Assets = []MediaAsset{{ID: "asset", Locator: "media/asset", Kind: MediaVideo, MIMEType: "video/mp4"}}
			if e = repo.SaveVideoJob(ctx, j, 2); e != nil {
				t.Fatal(e)
			}
			copy, e = repo.GetVideoJob(ctx, o.Scope, o.ID)
			if e != nil || copy.Info.Usage.CostUSD == nil || copy.Info.Usage.CostUSD.String() != "0" || copy.Assets[0].Locator != "media/asset" {
				t.Fatalf("metadata lost %+v %v", copy, e)
			}
			copy.Info.Usage.Units["seconds"] = "999"
			fresh, _ := repo.GetVideoJob(ctx, o.Scope, o.ID)
			if fresh.Info.Usage.Units["seconds"] != "8" {
				t.Fatal("aliased record")
			}
			j.Revision = 4
			j.State = MediaJobRunning
			if e = repo.SaveVideoJob(ctx, j, 3); !errors.Is(e, ErrConflict) {
				t.Fatal("terminal overwritten")
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel()
			if _, e = repo.GetVideoJob(cancelCtx, o.Scope, o.ID); !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
		})
	}
}

type fakeVideo struct {
	posts     atomic.Int32
	gets      atomic.Int32
	submitErr error
	getErr    error
	block     chan struct{}
	running   bool
}

func (f *fakeVideo) SubmitVideo(ctx context.Context, r VideoRequest) (VideoJob, error) {
	f.posts.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return VideoJob{}, ctx.Err()
		}
	}
	return VideoJob{Info: MediaResultInfo{Operation: r.Operation, Provider: "fixture", Model: "video"}, ProviderJobID: "job-1", State: MediaJobQueued}, f.submitErr
}
func (f *fakeVideo) GetVideo(ctx context.Context, j VideoJob) (VideoJob, error) {
	f.gets.Add(1)
	if f.getErr != nil {
		return j, f.getErr
	}
	j.State = MediaJobSucceeded
	if f.running {
		j.State = MediaJobRunning
	}
	j.OutputCount = 1
	return j, nil
}
func (f *fakeVideo) OpenVideo(ctx context.Context, j VideoJob, index int) (MediaContent, error) {
	return MediaContent{Asset: MediaAsset{Kind: MediaVideo, MIMEType: "video/mp4"}, Data: []byte("video")}, nil
}
func videoService(t *testing.T, f VideoGenerator, repo MediaJobRepository) *VideoService {
	t.Helper()
	s, e := NewVideoService(VideoServiceConfig{Generator: f, Jobs: repo})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestVideoServiceRestartAndReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	store, e := NewSQLiteStore(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	f := &fakeVideo{}
	service := videoService(t, f, store.MediaJobs())
	r := VideoRequest{Operation: mediaOp(), Prompt: "draw video"}
	j, e := service.Submit(ctx, r)
	if e != nil || j.ProviderJobID != "job-1" {
		t.Fatal(e)
	}
	_ = store.Close()
	store, e = NewSQLiteStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = store.Close() }()
	service = videoService(t, f, store.MediaJobs())
	if _, e = service.Submit(ctx, r); e != nil {
		t.Fatal(e)
	}
	j, e = service.Wait(ctx, r.Operation, MediaWaitOptions{Timeout: time.Second})
	if e != nil || j.State != MediaJobSucceeded {
		t.Fatalf("resume %+v %v", j, e)
	}
	if _, e = service.Submit(ctx, r); e != nil {
		t.Fatal(e)
	}
	if f.posts.Load() != 1 {
		t.Fatal("regenerated video")
	}
	r.Prompt = "different"
	if _, e = service.Submit(ctx, r); !errors.Is(e, ErrConflict) {
		t.Fatal("identity reused for different prompt")
	}
	if _, e = service.Open(ctx, r.Operation, 0); e != nil {
		t.Fatal(e)
	}
}
func TestVideoServiceAmbiguousNeverResubmits(t *testing.T) {
	f := &fakeVideo{submitErr: context.DeadlineExceeded}
	s := videoService(t, f, NewMemoryStore().MediaJobs())
	r := VideoRequest{Operation: mediaOp(), Prompt: "video"}
	j, e := s.Submit(context.Background(), r)
	if e == nil || j.State != MediaJobAmbiguous {
		t.Fatalf("%+v %v", j, e)
	}
	if _, e = s.Submit(context.Background(), r); e == nil {
		t.Fatal("ambiguous replay accepted")
	}
	if f.posts.Load() != 1 {
		t.Fatal("resubmitted ambiguous work")
	}
}
func TestVideoConcurrentReservation(t *testing.T) {
	f := &fakeVideo{block: make(chan struct{})}
	s := videoService(t, f, NewMemoryStore().MediaJobs())
	r := VideoRequest{Operation: mediaOp(), Prompt: "video"}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Submit(context.Background(), r) }()
	}
	time.AfterFunc(20*time.Millisecond, func() { close(f.block) })
	wg.Wait()
	if f.posts.Load() != 1 {
		t.Fatalf("submissions %d", f.posts.Load())
	}
}
func TestVideoWaitCancellationAndRetryBudget(t *testing.T) {
	f := &fakeVideo{running: true}
	s := videoService(t, f, NewMemoryStore().MediaJobs())
	o := mediaOp()
	if _, e := s.Submit(context.Background(), VideoRequest{Operation: o, Prompt: "video"}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	j, e := s.Wait(ctx, o, MediaWaitOptions{Interval: time.Millisecond})
	if !errors.Is(e, context.DeadlineExceeded) || j.State != MediaJobRunning {
		t.Fatalf("cancel %+v %v", j, e)
	}
	if _, e = s.Cancel(context.Background(), o); e == nil {
		t.Fatal("pretended remote cancel")
	}
	f.getErr = &MediaError{Kind: MediaErrorRateLimit, Retryable: true}
	before := f.gets.Load()
	_, e = s.Wait(context.Background(), o, MediaWaitOptions{Interval: time.Millisecond, RetryBudget: 2})
	if e == nil || f.gets.Load()-before != 3 {
		t.Fatal("unbounded retry")
	}
}

type cancellingVideo struct{ *fakeVideo }

func (f cancellingVideo) CancelVideo(ctx context.Context, j VideoJob) (VideoJob, error) {
	j.State = MediaJobCancelled
	return j, nil
}
func TestVideoRemoteCancelAndTerminalAttempts(t *testing.T) {
	store := NewMemoryStore()
	f := cancellingVideo{&fakeVideo{}}
	s, e := NewVideoService(VideoServiceConfig{Generator: f, Jobs: store.MediaJobs(), Attempts: store.ModelAttempts()})
	if e != nil {
		t.Fatal(e)
	}
	o := mediaOp()
	_, e = s.Submit(context.Background(), VideoRequest{Operation: o, Prompt: "video"})
	if e != nil {
		t.Fatal(e)
	}
	j, e := s.Cancel(context.Background(), o)
	if e != nil || j.State != MediaJobCancelled {
		t.Fatal(e)
	}
	for range 2 {
		if _, e = s.Get(context.Background(), o); e != nil {
			t.Fatal(e)
		}
	}
	records, e := store.ListModelAttempts(context.Background(), ModelAttemptFilter{}, PageRequest{})
	if e != nil || len(records.Records) != 1 {
		t.Fatalf("duplicate usage %+v %v", records, e)
	}
}

func TestVideoWaitTerminalStateKinds(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryStore()
	s := videoService(t, &fakeVideo{}, repo)
	o := mediaOp()
	if _, e := s.Submit(ctx, VideoRequest{Operation: o, Prompt: "video"}); e != nil {
		t.Fatal(e)
	}
	job, e := repo.GetVideoJob(ctx, o.Scope, o.ID)
	if e != nil || job.State != MediaJobQueued {
		t.Fatal(job, e)
	}
	job.State = MediaJobExpired
	job.Revision = 4
	if e = repo.SaveVideoJob(ctx, job, 3); e != nil {
		t.Fatal(e)
	}
	_, e = s.Wait(ctx, o, MediaWaitOptions{Timeout: time.Second})
	var me *MediaError
	if !errors.As(e, &me) || me.Kind != MediaErrorExpired {
		t.Fatalf("expired wait kind %v", e)
	}
	cs := videoService(t, cancellingVideo{&fakeVideo{}}, NewMemoryStore())
	o2 := MediaOperation{ID: "cancel-op", Scope: o.Scope}
	if _, e = cs.Submit(ctx, VideoRequest{Operation: o2, Prompt: "video"}); e != nil {
		t.Fatal(e)
	}
	if _, e = cs.Cancel(ctx, o2); e != nil {
		t.Fatal(e)
	}
	_, e = cs.Wait(ctx, o2, MediaWaitOptions{Timeout: time.Second})
	if !errors.As(e, &me) || me.Kind != MediaErrorCancelled {
		t.Fatalf("cancelled wait kind %v", e)
	}
}

func TestVideoCancelRejectsUnconfirmedJob(t *testing.T) {
	for _, state := range []MediaJobState{MediaJobSubmitting, MediaJobContacting} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			repo := NewMemoryStore()
			s := videoService(t, cancellingVideo{&fakeVideo{}}, repo)
			o := mediaOp()
			now := time.Now().UTC()
			job := VideoJob{Info: MediaResultInfo{Operation: o}, State: state, CreatedAt: now, UpdatedAt: now, Revision: 1}
			if e := repo.SaveVideoJob(ctx, job, 0); e != nil {
				t.Fatal(e)
			}
			if _, e := s.Cancel(ctx, o); e == nil {
				t.Fatal("cancelled job without provider identity")
			}
		})
	}
}

func TestRecordMediaAttemptCancellationClassification(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	o := mediaOp()
	deadline := MediaOperation{ID: "deadline-op", Scope: o.Scope}
	if e := RecordMediaAttempt(ctx, store.ModelAttempts(), MediaResultInfo{Operation: o, Provider: "fixture", Model: "m"}, "image", time.Now(), time.Now(), context.DeadlineExceeded); e != nil {
		t.Fatal(e)
	}
	if e := RecordMediaAttempt(ctx, store.ModelAttempts(), MediaResultInfo{Operation: deadline, Provider: "fixture", Model: "m"}, "image", time.Now(), time.Now(), &MediaError{Kind: MediaErrorCancelled, Message: "cancelled"}); e != nil {
		t.Fatal(e)
	}
	records, e := store.ListModelAttempts(ctx, ModelAttemptFilter{}, PageRequest{})
	if e != nil || len(records.Records) != 2 {
		t.Fatalf("records %+v %v", records, e)
	}
	for _, r := range records.Records {
		if r.Status != ModelAttemptCancelled || r.ErrorKind != "cancelled" {
			t.Fatalf("misclassified %+v", r)
		}
	}
}

type mediaSink struct {
	data    []byte
	locator string
}

func (s *mediaSink) PutMedia(ctx context.Context, o MediaOperation, a MediaAsset, r io.Reader) (MediaAsset, error) {
	b, e := io.ReadAll(r)
	s.data = b
	a.ID = "asset-1"
	a.Locator = s.locator
	if a.Locator == "" {
		a.Locator = "media/asset-1"
	}
	return a, e
}

type imageFake struct{}

func (imageFake) GenerateImages(ctx context.Context, r ImageRequest) (ImageResult, error) {
	return ImageResult{Info: MediaResultInfo{Operation: r.Operation}, Images: []MediaContent{{Asset: MediaAsset{Kind: MediaImage, MIMEType: "image/png"}, Data: []byte("binary-image")}}}, nil
}
func TestMediaImageToolReturnsReferences(t *testing.T) {
	sink := &mediaSink{}
	tool, e := NewImageTool(ImageToolConfig{Generator: imageFake{}, Sink: sink, Operation: func(context.Context) (MediaOperation, error) { return mediaOp(), nil }})
	if e != nil {
		t.Fatal(e)
	}
	out, e := tool.Execute(context.Background(), json.RawMessage(`{"prompt":"draw"}`))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(out), "binary-image") || !strings.Contains(string(out), "media/asset-1") {
		t.Fatal(string(out))
	}
	var result MediaArtifactResult
	if e = json.Unmarshal(out, &result); e != nil {
		t.Fatal(e)
	}
	if result.Assets[0].Bytes != 12 || len(result.Assets[0].SHA256) != 64 {
		t.Fatalf("bad asset %+v", result)
	}
	if _, e = tool.Execute(context.Background(), json.RawMessage(`{"prompt":"draw","tenant":"attack"}`)); e == nil {
		t.Fatal("model tenant accepted")
	}
}
func TestSaveMediaBoundedAndExplicit(t *testing.T) {
	c := MediaContent{Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/mpeg"}, Data: []byte("12345")}
	sink := &mediaSink{}
	if _, e := SaveMediaAsset(context.Background(), mediaOp(), c, sink, nil, 4); e == nil {
		t.Fatal("unbounded copy")
	}
	c.Data = nil
	c.TemporaryURL = "https://example.com/?secret=1"
	if _, e := SaveMediaAsset(context.Background(), mediaOp(), c, sink, nil, 10); e == nil {
		t.Fatal("implicitly fetched URL")
	}
	opened := false
	opener := func(context.Context, MediaOperation, MediaContent) (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("audio")), nil
	}
	a, e := SaveMediaAsset(context.Background(), mediaOp(), c, sink, opener, 10)
	if e != nil || !opened || a.Locator == "" {
		t.Fatal(e)
	}
	sink.locator = "https://example.com/?secret=1"
	if _, e = SaveMediaAsset(context.Background(), mediaOp(), c, sink, opener, 10); e == nil {
		t.Fatal("persisted signed URL")
	}
	if _, e = json.Marshal(c); e == nil {
		t.Fatal("serialized ephemeral content")
	}
}
func TestCopyMediaBoundedAndCancelled(t *testing.T) {
	var dst bytes.Buffer
	n, e := CopyMedia(context.Background(), &dst, strings.NewReader("12345"), 5)
	if e != nil || n != 5 {
		t.Fatal(n, e)
	}
	dst.Reset()
	if _, e = CopyMedia(context.Background(), &dst, strings.NewReader("123456"), 5); e == nil {
		t.Fatal("overflow")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = CopyMedia(ctx, &dst, strings.NewReader("a"), 10); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

type faultJobs struct {
	MediaJobRepository
	failRead bool
	failSave int64
}

func (f faultJobs) GetVideoJob(ctx context.Context, s RuntimeScope, id string) (VideoJob, error) {
	if f.failRead {
		return VideoJob{}, errors.New("storage unavailable")
	}
	return f.MediaJobRepository.GetVideoJob(ctx, s, id)
}
func (f faultJobs) SaveVideoJob(ctx context.Context, j VideoJob, revision int64) error {
	if revision == f.failSave {
		return errors.New("storage unavailable")
	}
	return f.MediaJobRepository.SaveVideoJob(ctx, j, revision)
}
func TestVideoStorageFailureSafety(t *testing.T) {
	for _, phase := range []string{"read", "reserve", "contacting", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			f := &fakeVideo{}
			repo := faultJobs{MediaJobRepository: NewMemoryStore(), failSave: -1}
			if phase == "read" {
				repo.failRead = true
			}
			if phase == "reserve" {
				repo.failSave = 0
			}
			if phase == "contacting" {
				repo.failSave = 1
			}
			if phase == "accepted" {
				repo.failSave = 2
			}
			s := videoService(t, f, repo)
			j, e := s.Submit(context.Background(), VideoRequest{Operation: mediaOp(), Prompt: "video"})
			if e == nil {
				t.Fatal("lost storage error")
			}
			if phase == "accepted" {
				if j.ProviderJobID != "job-1" || f.posts.Load() != 1 {
					t.Fatal("lost accepted handle")
				}
			} else if f.posts.Load() != 0 {
				t.Fatal("submitted before durable reservation")
			}
		})
	}
}

func requestHash(r VideoRequest) string {
	body, _ := json.Marshal(r)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestVideoSubmitResumesPreContactReservation(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryStore()
	f := &fakeVideo{}
	s := videoService(t, f, repo)
	o := mediaOp()
	r := VideoRequest{Operation: o, Prompt: "video"}
	now := time.Now().UTC()
	orphan := VideoJob{Info: MediaResultInfo{Operation: o}, State: MediaJobSubmitting, CreatedAt: now, UpdatedAt: now, Revision: 1, RequestHash: requestHash(r)}
	if e := repo.SaveVideoJob(ctx, orphan, 0); e != nil {
		t.Fatal(e)
	}
	j, e := s.Submit(ctx, r)
	if e != nil || j.ProviderJobID != "job-1" || j.State != MediaJobQueued || f.posts.Load() != 1 {
		t.Fatalf("pre-contact resume %+v %v", j, e)
	}
	stored, e := repo.GetVideoJob(ctx, o.Scope, o.ID)
	if e != nil || stored.ProviderJobID != "job-1" || stored.State != MediaJobQueued {
		t.Fatalf("durable resume %+v %v", stored, e)
	}
	if _, e = s.Submit(ctx, r); e != nil {
		t.Fatal(e)
	}
	if f.posts.Load() != 1 {
		t.Fatal("resubmitted completed work")
	}
}

func TestVideoSubmitRefusesPostContactOrphans(t *testing.T) {
	for _, state := range []MediaJobState{MediaJobContacting, MediaJobAmbiguous} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			repo := NewMemoryStore()
			f := &fakeVideo{}
			s := videoService(t, f, repo)
			o := mediaOp()
			r := VideoRequest{Operation: o, Prompt: "video"}
			now := time.Now().UTC()
			orphan := VideoJob{Info: MediaResultInfo{Operation: o}, State: state, CreatedAt: now, UpdatedAt: now, Revision: 1, RequestHash: requestHash(r)}
			if e := repo.SaveVideoJob(ctx, orphan, 0); e != nil {
				t.Fatal(e)
			}
			if _, e := s.Submit(ctx, r); e == nil {
				t.Fatalf("resubmitted %s orphan", state)
			}
			if f.posts.Load() != 0 {
				t.Fatal("contacted provider for ambiguous outcome")
			}
		})
	}
}

func TestVideoSubmitConflictDoesNotResumeForeignReservation(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryStore()
	f := &fakeVideo{}
	s := videoService(t, f, repo)
	o := mediaOp()
	now := time.Now().UTC()
	orphan := VideoJob{Info: MediaResultInfo{Operation: o}, State: MediaJobSubmitting, CreatedAt: now, UpdatedAt: now, Revision: 1, RequestHash: requestHash(VideoRequest{Operation: o, Prompt: "different"})}
	if e := repo.SaveVideoJob(ctx, orphan, 0); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Submit(ctx, VideoRequest{Operation: o, Prompt: "video"}); !errors.Is(e, ErrConflict) {
		t.Fatalf("accepted different request on orphan %v", e)
	}
	if f.posts.Load() != 0 {
		t.Fatal("contacted provider for conflicting request")
	}
}
func TestMediaToolsTrustedContextAndVideo(t *testing.T) {
	ctx := context.Background()
	ctx = withWorkflowInvocation(ctx, "run", 1, "step", "trusted-thread", nil)
	ctx = context.WithValue(ctx, toolMetadataContextKey{}, map[string]string{"run_id": "run", "tool_call_id": "call", "thread_id": "forged"})
	ctx = WithRuntimeScope(ctx, mediaOp().Scope)
	o, e := MediaOperationFromToolContext(ctx)
	if e != nil || o.ThreadID != "trusted-thread" {
		t.Fatal(o, e)
	}
	if _, e = MediaOperationFromToolContext(context.Background()); e == nil {
		t.Fatal("accepted no invocation")
	}
	f := &fakeVideo{}
	tool, e := NewVideoTool(VideoToolConfig{Service: videoService(t, f, NewMemoryStore())})
	if e != nil {
		t.Fatal(e)
	}
	if !json.Valid(tool.Definition().InputSchema) || tool.Definition().ID != "generate_video" {
		t.Fatal("bad tool definition")
	}
	b, e := tool.Execute(ctx, json.RawMessage(`{"prompt":"video"}`))
	if e != nil {
		t.Fatal(e)
	}
	var job VideoJob
	if json.Unmarshal(b, &job) != nil || job.ProviderJobID != "job-1" {
		t.Fatal(string(b))
	}
	if _, e = tool.Execute(ctx, json.RawMessage(`{}`)); e == nil {
		t.Fatal("empty video prompt")
	}
	imageTool, e := NewImageTool(ImageToolConfig{Generator: imageFake{}, Sink: &mediaSink{}})
	if e != nil {
		t.Fatal(e)
	}
	if !json.Valid(imageTool.Definition().OutputSchema) {
		t.Fatal("bad output schema")
	}
	if _, e = imageTool.Execute(ctx, json.RawMessage(`{"prompt":"image"}`)); e != nil {
		t.Fatal(e)
	}
	if _, e = NewVideoTool(VideoToolConfig{}); e == nil {
		t.Fatal("nil video service")
	}
	if _, e = NewImageTool(ImageToolConfig{}); e == nil {
		t.Fatal("nil image generator")
	}
	var nilVideo *fakeVideo
	if _, e = NewVideoService(VideoServiceConfig{Generator: nilVideo, Jobs: NewMemoryStore()}); e == nil {
		t.Fatal("typed-nil generator accepted")
	}
	var nilJobs MediaJobRepository
	if _, e = NewVideoService(VideoServiceConfig{Generator: &fakeVideo{}, Jobs: nilJobs}); e == nil {
		t.Fatal("typed-nil job repository accepted")
	}
	var nilSink *mediaSink
	if _, e = NewImageTool(ImageToolConfig{Generator: imageFake{}, Sink: nilSink}); e == nil {
		t.Fatal("typed-nil sink accepted")
	}
	if _, e = NewImageTool(ImageToolConfig{Generator: imageFake{}, Sink: &mediaSink{}, MaxBytes: -1}); e == nil {
		t.Fatal("negative image limit")
	}
}
func TestMediaAssetWorkflowResume(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	calls := 0
	var reference json.RawMessage
	steps := []Step{{Definition: StepDefinition{ID: "generate"}, Handler: StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls++
		reference, _ = json.Marshal(MediaArtifactResult{Operation: mediaOp(), Assets: []MediaAsset{{ID: "asset", Locator: "images/asset", Kind: MediaImage, MIMEType: "image/png"}}})
		return reference, nil
	})}, {Definition: StepDefinition{ID: "wait", SuspendSchema: json.RawMessage(`{}`)}, Handler: StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, &SuspendError{Signal: SuspendSignal{Contract: json.RawMessage(`{}`)}}
	})}, {Definition: StepDefinition{ID: "finish"}, Handler: StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"done":true}`), nil
	})}}
	config := LinearWorkflowConfig{Definition: WorkflowDefinition{ID: "media-workflow", Version: "1"}, SchemaCompiler: contractCompiler(), Steps: steps, Store: store}
	wf, e := NewLinearWorkflow(config)
	if e != nil {
		t.Fatal(e)
	}
	result, e := wf.Run(ctx, WorkflowRunInput{Input: json.RawMessage(`{}`)})
	if e != nil || result.Status != RunStatusSuspended {
		t.Fatal(result, e)
	}
	wf, e = NewLinearWorkflow(config)
	if e != nil {
		t.Fatal(e)
	}
	resumed, e := wf.Resume(ctx, WorkflowResumeInput{RunID: result.ID, Input: json.RawMessage(`{}`)})
	if e != nil || resumed.Status != RunStatusSucceeded || calls != 1 {
		t.Fatalf("regenerated on resume %d %+v %v", calls, resumed, e)
	}
	record, e := store.GetWorkflowRun(ctx, result.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(record.StepOutputs) == 0 || string(record.StepOutputs[0]) != string(reference) {
		t.Fatal("lost artifact checkpoint")
	}
}
func TestInvalidMediaRecordsAndScope(t *testing.T) {
	now := time.Now().UTC()
	base := VideoJob{Info: MediaResultInfo{Operation: mediaOp()}, Revision: 1, State: MediaJobSubmitting, CreatedAt: now, UpdatedAt: now}
	repo := NewMemoryStore()
	for _, mutate := range []func(*VideoJob){func(j *VideoJob) { j.Info.Operation.ID = "" }, func(j *VideoJob) { j.Info.Operation.ID = strings.Repeat("x", 257) }, func(j *VideoJob) { j.Revision = 0 }, func(j *VideoJob) { j.State = "bad" }, func(j *VideoJob) { j.ProviderJobID = "https://secret" }, func(j *VideoJob) { j.Assets = []MediaAsset{{ID: "a", Locator: "https://signed?key=secret"}} }, func(j *VideoJob) { j.RequestHash = strings.Repeat("x", 65<<10) }} {
		j := base
		mutate(&j)
		if e := repo.SaveVideoJob(context.Background(), j, 0); e == nil {
			t.Fatal("accepted invalid record")
		}
	}
	if e := repo.SaveVideoJob(WithRuntimeScope(context.Background(), RuntimeScope{Namespace: "different"}), base, 0); e == nil {
		t.Fatal("cross-scope write")
	}
	f := &fakeVideo{}
	s := videoService(t, f, repo)
	badScope := WithRuntimeScope(context.Background(), RuntimeScope{Namespace: "different"})
	if _, e := s.Submit(badScope, VideoRequest{Operation: mediaOp(), Prompt: "video"}); e == nil {
		t.Fatal("cross-scope submit")
	}
	if _, e := s.Get(context.Background(), mediaOp()); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e := s.Open(context.Background(), mediaOp(), 0); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e := NewVideoService(VideoServiceConfig{}); e == nil {
		t.Fatal("nil service config")
	}
	if _, e := s.Wait(context.Background(), mediaOp(), MediaWaitOptions{Jitter: 2}); e == nil {
		t.Fatal("invalid jitter")
	}
	if _, e := s.Wait(context.Background(), mediaOp(), MediaWaitOptions{Interval: time.Second, MaxInterval: time.Millisecond}); e == nil {
		t.Fatal("invalid intervals")
	}
}

type syntheticMediaReader struct{}

func (syntheticMediaReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
func BenchmarkCopyMedia(b *testing.B) {
	const size = 32 << 20
	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		if _, e := CopyMedia(context.Background(), io.Discard, io.LimitReader(syntheticMediaReader{}, size), size); e != nil {
			b.Fatal(e)
		}
	}
}
func TestVideoFailedSubmissionAndCancelRace(t *testing.T) {
	f := &fakeVideo{submitErr: &MediaError{Kind: MediaErrorRefusal, Message: "refused"}}
	store := NewMemoryStore()
	s := videoService(t, f, store)
	o := mediaOp()
	j, e := s.Submit(context.Background(), VideoRequest{Operation: o, Prompt: "video"})
	if e == nil || j.State != MediaJobFailed {
		t.Fatal(j, e)
	}
	if _, e = s.Wait(context.Background(), o, MediaWaitOptions{}); e == nil {
		t.Fatal("failed job reported success")
	}
	s.config.Attempts = store.ModelAttempts()
	if _, e = s.Get(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Open(context.Background(), o, 0); e == nil {
		t.Fatal("opened failed result")
	}
}

type errMediaReader struct {
	err error
	n   int
}

func (r errMediaReader) Read(b []byte) (int, error) { return r.n, r.err }

type shortMediaWriter struct{}

func (shortMediaWriter) Write(b []byte) (int, error) { return 0, nil }
func TestMediaIOAndValidationFailures(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("source failed")
	if _, e := CopyMedia(ctx, io.Discard, errMediaReader{err: sentinel}, 10); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	if _, e := CopyMedia(ctx, io.Discard, errMediaReader{}, 10); !errors.Is(e, io.ErrNoProgress) {
		t.Fatal(e)
	}
	if _, e := CopyMedia(ctx, shortMediaWriter{}, strings.NewReader("x"), 10); !errors.Is(e, io.ErrShortWrite) {
		t.Fatal(e)
	}
	if _, e := CopyMedia(ctx, io.Discard, errMediaReader{n: -1}, 10); e == nil {
		t.Fatal("invalid reader count")
	}
	if _, e := CopyMedia(ctx, io.Discard, strings.NewReader("x"), 0); e == nil {
		t.Fatal("invalid limit")
	}
	if n, e := CopyMedia(ctx, io.Discard, strings.NewReader("x"), math.MaxInt64); e != nil || n != 1 {
		t.Fatalf("max-int64 limit %d %v", n, e)
	}
	negative := -1.5
	positive := 1.5
	nan := math.NaN()
	for _, c := range []MediaContent{{}, {Data: []byte{}}, {Data: []byte("x"), Reader: io.NopCloser(strings.NewReader("x"))}, {Data: []byte("x"), Asset: MediaAsset{Kind: "invalid"}}, {Data: []byte("x"), Asset: MediaAsset{Kind: MediaAudio, MIMEType: "image/png"}}, {Data: []byte("x"), Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/wav", Bytes: -1}}, {Data: []byte("x"), Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/wav", SampleRate: -1, Channels: -2}}, {Data: []byte("x"), Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/wav", DurationSeconds: &negative}}, {Data: []byte("x"), Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/wav", DurationSeconds: &nan}}} {
		if e := c.Validate(); e == nil {
			t.Fatal("invalid content accepted")
		}
	}
	if e := (MediaContent{Data: []byte("x"), Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/wav", SampleRate: 24000, Channels: 1, DurationSeconds: &positive}}).Validate(); e != nil {
		t.Fatal(e)
	}
	if _, e := SaveMediaAsset(ctx, mediaOp(), MediaContent{Asset: MediaAsset{Kind: MediaAudio, MIMEType: "audio/wav"}, Reader: io.NopCloser(strings.NewReader(""))}, &mediaSink{}, nil, 64<<20); e == nil {
		t.Fatal("empty media accepted")
	}
	e := &MediaError{Kind: MediaErrorTransport, Message: "safe", Cause: sentinel}
	if !errors.Is(e, sentinel) || strings.Contains(e.Error(), "source failed") {
		t.Fatal("unsafe error wrapper")
	}
	if e := (MediaOperation{ID: "bad\nidentity"}).Validate(); e == nil {
		t.Fatal("invalid identity accepted")
	}
	q := (&sqlMediaJobs{postgres: true}).bind("UPDATE media_jobs SET revision=? WHERE namespace=? AND owner_id=? AND id=?")
	if q != "UPDATE media_jobs SET revision=$1 WHERE namespace=$2 AND owner_id=$3 AND id=$4" {
		t.Fatal(q)
	}
	q = (&sqlMediaJobs{postgres: true}).bind(strings.Repeat("?,", 12) + "?")
	want := "$" + strings.Join([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13"}, ",$")
	if q != want {
		t.Fatalf("placeholder numbering: got %s want %s", q, want)
	}
}

type noReadMediaSink struct{}

func (noReadMediaSink) PutMedia(context.Context, MediaOperation, MediaAsset, io.Reader) (MediaAsset, error) {
	return MediaAsset{ID: "a", Locator: "a"}, nil
}

type failingImages struct{ err error }

func (f failingImages) GenerateImages(context.Context, ImageRequest) (ImageResult, error) {
	return ImageResult{}, f.err
}
func TestAssetPersistenceFailuresCannotReturnSuccess(t *testing.T) {
	ctx := context.Background()
	o := mediaOp()
	c := MediaContent{Asset: MediaAsset{Kind: MediaImage, MIMEType: "image/png"}, Data: []byte("image")}
	if _, e := SaveMediaAsset(ctx, o, c, noReadMediaSink{}, nil, 10); e == nil {
		t.Fatal("sink skipped data")
	}
	if _, e := SaveMediaAsset(ctx, o, c, nil, nil, 10); e == nil {
		t.Fatal("nil sink")
	}
	if _, e := SaveMediaAsset(ctx, MediaOperation{}, c, &mediaSink{}, nil, 10); e == nil {
		t.Fatal("missing identity")
	}
	if _, e := SaveMediaAsset(ctx, o, MediaContent{}, &mediaSink{}, nil, 10); e == nil {
		t.Fatal("missing media")
	}
	c.Data = nil
	c.TemporaryURL = "https://application-controlled.example"
	sentinel := errors.New("storage failure")
	if _, e := SaveMediaAsset(ctx, o, c, &mediaSink{}, func(context.Context, MediaOperation, MediaContent) (io.ReadCloser, error) { return nil, sentinel }, 10); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	if _, e := SaveMediaAsset(ctx, o, c, &mediaSink{}, func(context.Context, MediaOperation, MediaContent) (io.ReadCloser, error) { return nil, nil }, 10); e == nil {
		t.Fatal("nil reader accepted")
	}
	for _, failure := range []error{nil, sentinel} {
		tool, e := NewImageTool(ImageToolConfig{Generator: failingImages{err: failure}, Sink: &mediaSink{}, Operation: func(context.Context) (MediaOperation, error) { return o, nil }})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tool.Execute(ctx, json.RawMessage(`{"prompt":"image"}`)); e == nil {
			t.Fatal("empty/failed generation accepted")
		}
	}
	tool, e := NewImageTool(ImageToolConfig{Generator: imageFake{}, Sink: &mediaSink{}, Operation: func(context.Context) (MediaOperation, error) { return o, sentinel }})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tool.Execute(ctx, json.RawMessage(`{"prompt":"image"}`)); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	videoTool, e := NewVideoTool(VideoToolConfig{Service: videoService(t, &fakeVideo{}, NewMemoryStore())})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = videoTool.Execute(ctx, json.RawMessage(`{"prompt":"video"}`)); e == nil {
		t.Fatal("unscoped tool invocation accepted")
	}
}

type capturingImages struct{ request ImageRequest }

func (g *capturingImages) GenerateImages(ctx context.Context, r ImageRequest) (ImageResult, error) {
	g.request = r
	return (imageFake{}).GenerateImages(ctx, r)
}
func TestImageToolOptions(t *testing.T) {
	for _, allow := range []bool{false, true} {
		g := &capturingImages{}
		tool, err := NewImageTool(ImageToolConfig{Generator: g, Sink: &mediaSink{}, Defaults: ImageOptions{Count: 2, Quality: "high", Format: "png"}, AllowModelOptions: allow, Operation: func(context.Context) (MediaOperation, error) { return mediaOp(), nil }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tool.Execute(context.Background(), json.RawMessage(`{"prompt":"draw"}`)); err != nil {
			t.Fatal(err)
		}
		if g.request.Count != 2 || g.request.Quality != "high" || g.request.Format != "png" {
			t.Fatalf("defaults lost: %+v", g.request)
		}
		args := json.RawMessage(`{"prompt":"draw","count":3,"quality":"low","format":"jpeg","resolution":"1K","aspect_ratio":"16:9"}`)
		_, err = tool.Execute(context.Background(), args)
		if !allow {
			if err == nil {
				t.Fatal("model options enabled without opt-in")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if g.request.Count != 3 || g.request.Quality != "low" || g.request.Format != "jpeg" || g.request.Resolution != "1K" || g.request.AspectRatio != "16:9" {
			t.Fatalf("overrides lost: %+v", g.request)
		}
		for _, invalid := range []string{`{"prompt":"draw","count":0}`, `{"prompt":"draw","count":1.5}`, `{"prompt":"draw","size":"1024x1024","aspect_ratio":"16:9"}`, `{"prompt":"draw","operation":"spoof"}`, `{"prompt":"draw"} {}`} {
			if _, err = tool.Execute(context.Background(), json.RawMessage(invalid)); err == nil {
				t.Fatalf("accepted %s", invalid)
			}
		}
		if _, err = tool.Execute(context.Background(), json.RawMessage(`{"prompt":"draw","size":"1024x1024"}`)); err != nil {
			t.Fatal(err)
		}
		if g.request.Size != "1024x1024" {
			t.Fatal("size lost")
		}
	}
}
