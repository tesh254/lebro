# Media generation and speech input

Lebro media APIs work directly or as agent tools. Use `openai.NewMedia` or
`openrouter.NewMedia` with a media model; the existing chat `Model` interface is
unchanged. A chat model chooses tools; the configured media provider performs
generation. An application that only needs prompt → image/video can call the
capability directly without paying for a chat planning call.

## Provider capabilities

Documentation verified 2026-09-10. These are the initial built-in profiles;
model availability and access must be verified with your own provider account.

| Adapter/model | Supported path | Limits/options |
| --- | --- | --- |
| OpenAI `gpt-image-1`, `gpt-image-1.5` | Text → images | 1–10 images; PNG/JPEG/WebP; documented sizes and quality |
| OpenRouter `openai/gpt-image-1`, `openai/gpt-image-1.5` | Dedicated `/images` | Same conservative image controls; fallback disabled |
| OpenRouter `google/veo-3.1` | `/videos`, status, content | 4/6/8 seconds; 720p/1080p; landscape/portrait; 1080p requires explicit 8 seconds |
| OpenAI `whisper-1` | Multipart file transcription | Language/prompt hints, optional segments/word timestamps |
| OpenRouter `openai/whisper-1`, `openai/whisper-large-v3` | Multipart transcription | Language and timestamps; prompt hints rejected because the gateway ignores them |
| OpenAI/OpenRouter `gpt-4o-transcribe`, `gpt-4o-mini-transcribe` | Multipart transcription | Text; timing requests rejected by the profile; OpenRouter model names use `openai/` |
| OpenAI `gpt-live-transcribe` | Live WebSocket transcription | PCM16 little endian, 24 kHz, mono; one utterance per call |
| OpenAI/OpenRouter `gpt-4o-mini-tts` | Speech reader or streaming callback | Explicit provider voice; MP3 or PCM; speed 0.25–4 |

`Capabilities()` returns implemented modes and limits. Unknown models advertise
no capabilities. Applications may supply an explicit `MediaConfig.Capabilities`
profile after verifying the selected model's endpoints; this does not add new
wire protocols. Empty option lists are unsupported/unknown, not unlimited.
Provider-neutral contracts do not claim that voices or formats are portable.

OpenRouter live input streaming, remote video cancellation, image editing/masks,
reference-conditioned video, progressive images, diarization controls, SSML,
voice cloning, alignment controls, webhooks and transcoding are not implemented
in this baseline. Optional result metadata is preserved when supplied. OpenAI
Sora is not included because its documented shutdown is 2026-09-24.

## An agent that generates images

```go
images, err := openrouter.NewMedia(openrouter.MediaConfig{
    APIKey: os.Getenv("OPENROUTER_API_KEY"), Model: "openai/gpt-image-1",
})
// Handle err after every call.
tool, err := lebro.NewImageTool(lebro.ImageToolConfig{
    Generator: images, Sink: applicationAssetSink,
    Defaults: lebro.ImageOptions{Count: 1, Quality: "high", Format: "png"},
    AllowModelOptions: true,
})
registry, err := lebro.NewToolRegistry(jsonschema.NewCompiler())
err = registry.Register(tool)
agent, err := lebro.NewAgent(lebro.AgentConfig{
    Definition: lebro.AgentDefinition{
        ID: "designer", Tools: []lebro.ToolID{tool.Definition().ID},
        Instructions: "Generate images using the tool; return its asset references.",
    },
    Model: chatModel, Tools: registry,
})
```

`Defaults` sets image options for omitted arguments. Set `AllowModelOptions: true`
to let the AI choose `count`, `size`, `resolution`, `aspect_ratio`, `quality`, and
`format` in its tool call. It is false by default, keeping application settings
fixed. Provider capability validation rejects unsupported values before sending
requests. Size cannot coexist with resolution/aspect ratio; an override can clear
a default size with `"size": ""`. Configure the generator's capability limits to
bound image count. Provider/model and operation identity remain application-owned.
Direct calls also accept these settings via `images.GenerateImages(ctx,
lebro.ImageRequest{Operation: operation, Prompt: "A mountain lake", Count: 1,
Size: "1024x1024", Quality: "high", Format: "png"})`.

`NewVideoTool` similarly wraps `VideoService`. Its tool output is a saved job
handle, so the application can show progress and poll independently of the agent
response. Tool operation identity comes from trusted run/tool-call context;
tenant/owner are never model arguments. Use `WithRuntimeScope` after authentication
and supply `Policy` to the service/tool/adapter to authorize direct calls too.

Generated image descriptors are JSON tool outputs. Large bytes, readers, and
temporary URLs cannot be serialized as `MediaContent`. This keeps existing
message formats and text/image/PDF history compatible. Workflow steps can return
`MediaArtifactResult` or `VideoJob` as ordinary JSON output. Completed checkpoint
outputs are reused on resume. A replay of a stored transcript does not invoke a
tool. Deliberately rerunning a new image tool call does generate a new image.

## Speaking into a prompt input

1. Capture microphone audio in the application. For recorded dictation, upload a
   supported WAV/MP3/M4A/WebM/Ogg/FLAC file to your authenticated backend.
2. Call `Transcribe` with a `MediaContent.Reader` and declared MIME type.
3. Put `TranscriptionResult.Text` into the editable prompt field. Submit only
   when your UX calls for it; transcription itself never starts an agent.

For live dictation, call `StreamTranscription` with an `AudioSource`. OpenAI's
initial profile accepts 24 kHz mono PCM16, in chunks no larger than 32 KiB.
`io.EOF` commits the utterance (minimum 100 ms). The callback receives complete
replacement hypotheses keyed by segment ID; replace displayed provisional text,
then settle it when `Final`/`Terminal` arrives. Do not append hypotheses.
Disconnect before the final event is an error. There is no reconnect/replay.

`voice.NewMediaVoice` adapts these providers to the existing `voice.Session`.
`Session.Turn` performs STT → agent → TTS, intentionally bypassing an editable
prompt step. Existing custom voice implementations remain compatible.

## Speech output and ownership

`SynthesizeSpeech` returns a `SpeechResult` whose `Audio.Reader` is a bounded
`io.ReadCloser`. Consume `Audio.Reader` to EOF and close it, including on failure.
`StreamSpeech` invokes a synchronous callback with at
most 32 KiB per chunk. Copy bytes before retaining them. Chunks belong to one
encoded stream; they are not independently playable files. Slow consumers
naturally apply backpressure. Cancel the context to stop network reads.

Input readers are consumed and closed by the transcription adapter; they must
unblock when closed. Application `AudioSource` and sink callbacks must honor
context cancellation. Lebro cannot interrupt arbitrary blocking application I/O.
The SDK validates recognizable container headers, declared formats and bounds;
full decoding/corruption detection belongs to the provider. It never silently
resamples, downmixes or splits long text. Silence is a valid empty transcript.
Missing confidence/timing/language stays unknown. Times are seconds from the
recording start; speaker labels are recording-local, not personal identities.

Default configured request timeout is five minutes; video wait defaults to
15 minutes. Live input uses per-message idle deadlines plus caller cancellation.
Default input limit is 25 MiB; output is 64 MiB, enforced while reading even when
HTTP content length is absent. Image provider JSON/base64 is bounded in memory;
audio/video have reader paths. Application server/proxy deadlines must accommodate
the operation. Set tighter limits for your product and test its actual recordings.

## Durable video and application assets

```go
store, err := lebro.NewSQLiteStore("jobs.db")
err = store.Migrate(ctx)
service, err := lebro.NewVideoService(lebro.VideoServiceConfig{
    Generator: videoProvider, Jobs: store.MediaJobs(),
    Attempts: store.ModelAttempts(), Policy: applicationPolicy,
})
op := lebro.MediaOperation{ID: applicationRequestID, Scope: verifiedScope}
job, err := service.Submit(ctx, lebro.VideoRequest{Operation: op, Prompt: prompt})
// After restart, use the same store, model configuration, scope and operation ID.
job, err = service.Wait(ctx, op, lebro.MediaWaitOptions{RetryBudget: 3, Jitter: .2})
content, err := service.Open(ctx, op, 0)
asset, err := lebro.SaveMediaAsset(ctx, op, content, applicationAssetSink, nil, 64<<20)
```

`MediaJobs()` is an optional extension on Memory, SQLite and Postgres stores;
custom stores need not change. Run migrations on existing SQL databases. It has
its own atomic compare-and-swap boundary and is not part of a legacy Store
transaction. Memory is process-local. Raw provider `SubmitVideo` calls return a
portable handle but leave persistence entirely to the caller.

A reserved operation that loses its submission response remains ambiguous.
Never retry it blindly: inspect the provider account and reconcile the handle
through `MediaJobRepository` using the saved revision. If the process dies
between acceptance and persisting the provider ID, the SDK cannot recover an ID
the provider never returned. Failed save errors return any known handle. This is
not an exactly-once guarantee for external providers.

Polling retries only transient/rate-limit errors within the configured budget,
honors retry hints and respects the overall deadline. It never retries a paid
submission. Cancelling a wait leaves remote work and billing unchanged. The
optional `VideoCanceller` contract requires confirmed cancellation; the baseline
OpenRouter adapter rejects it rather than equating deletion with cancellation.
Terminal writes are immutable and duplicate observations cannot re-add usage.

`SaveMediaAsset` copies into your atomic `MediaAssetSink`, measures bytes/checksum,
and requires a durable opaque locator. The SDK never follows arbitrary URLs.
Temporary references need an explicit `MediaAssetOpener` implementing your access,
host, redirect, private-network, size and timeout policy. Provider video content
is retrieved explicitly from its configured origin; redirects are rejected.
Persist the returned durable descriptor in application records. Your application
owns expiry, deletion, access control and cleanup of partial/orphaned objects.

## Usage, diagnostics and validation

Configure adapter `Attempts` and video-service `Attempts` to use existing durable
model-attempt repositories. Media provenance lives in bounded `media.operation`
metadata, correlated to operation/run/thread/scope. Direct calls use a synthetic
run ID. Speech usage is finalized when its reader reaches EOF or closes.
Native units are retained only when returned. `CostUSD == nil` means unavailable;
a non-nil decimal `"0"` is a reported zero. No price lookup or estimate runs.
Full monetary aggregation/resolvers remain MAD-84. Default SDK error text omits
provider payloads, prompts, recordings and signed URL credentials. Do not log
unwrapped transport errors or explicit application transcript results by default.

Fixture tests cover provider wire mapping, refusal/malformed responses, limits,
streaming, cancellation, durable jobs and scope boundaries without credentials.
Real-provider smoke runs are opt-in using the commands below; no paid requests
run in CI. Account access, actual media quality and provider latency are not
verified by fixture tests. Slow-consumer tests exercise 32 KiB output chunks;
measure production latency/throughput before selecting performance targets.

A 2026-09-10 Apple M1 benchmark (`go test ./internal/runtime -run '^$'
-bench BenchmarkCopyMedia -benchmem`) copied a 32 MiB synthetic stream into
`io.Discard` in about 0.77 ms with 32,799 bytes allocated (two allocations).
This verifies bounded copy allocation only; it does not measure network/provider
latency, real codec processing, disk throughput, or application playback.

## Runnable examples / opt-in smoke commands

Set credentials in your environment securely. Examples write to `media-output`
and a local `media-jobs.db`; outputs are not automatically published.

```sh
go run ./examples/media -mode image -provider openrouter -model openai/gpt-image-1
go run ./examples/media -mode image -provider openai -model gpt-image-1.5
go run ./examples/media -mode video -model google/veo-3.1 -operation video-001
go run ./examples/media -mode resume -model google/veo-3.1 -operation video-001
go run ./examples/media -mode transcribe -provider openai -model whisper-1 -input recording.wav
go run ./examples/media -mode live -provider openai -model gpt-live-transcribe -input utterance.pcm
go run ./examples/media -mode speak -model openai/gpt-4o-mini-tts -prompt 'Hello!'
go run ./examples/media -mode speak-stream -model openai/gpt-4o-mini-tts -prompt 'Hello!'
go run ./examples/media -mode agent -model openai/gpt-image-1 -chat-model YOUR_CHAT_MODEL
go run ./examples/media -mode voice -provider openai -model whisper-1 -input recording.wav -chat-model YOUR_CHAT_MODEL
```

Record provider/model/date and inspect the resulting media for every smoke run.

## What the integrating application still provides

- Provider credentials, model access, budgets, rate limits and model selection.
- Authenticated endpoints, trusted tenant/owner context and authorization policy.
- Microphone permission/capture, upload handling, optional explicit audio conversion,
  and an editable dictation prompt UI with provisional/final text handling.
- An atomic asset sink (filesystem/S3/R2/etc.), asset records, authorized downloads,
  playback/rendering, retention/deletion and orphan cleanup.
- SQL deployment/migrations, a worker to resume video polling, progress delivery
  to the UI, reconciliation of ambiguous submissions and asset expiration handling.
- Chat-agent instructions and registered image/video tools, or direct generation
  calls for a dedicated generator. Workflow checkpoints for multi-step work.
- Client cancellation, server/proxy streaming timeouts, disconnect cleanup, quotas,
  and observability consumers for usage/error records.
- Product decisions about when dictation submits, whether replies are spoken,
  generated-voice disclosure, sensitive-recording consent/retention, and provider
  data-policy requirements. Live-provider quality/access testing before rollout.

The SDK addition is additive and follows the repository's next-minor version
policy; it does not publish a release or deploy application infrastructure.
