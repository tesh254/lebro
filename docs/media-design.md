# MAD-115 media design

Status: implementation; provider documentation checked 2026-09-10.

Media is independent of chat `Model`. Small interfaces live in the runtime and
are aliased through the root façade. OpenAI and OpenRouter own their transports.
No new required methods on `Model`, `Store`, or `RuntimeStore`.

An asset descriptor is safe, bounded metadata; content is a separate, ephemeral
handle containing exactly one of bytes, a reader, a temporary provider URL, or a
durable application locator. Readers are owned and closed by the consumer.
Applications supply blob persistence and authenticated retrieval. The SDK never
automatically fetches a URL from model output. Provider content endpoints are
accessed explicitly, without following redirects or forwarding credentials to
another host. Applications own retention, deletion, and orphan reconciliation.

Generated artifacts travel as typed JSON tool outputs and workflow outputs,
not new chat message parts. Existing text/image/PDF transcripts remain unchanged.
Tools only return durable descriptors or operation IDs, never binary data or
temporary signed URLs. Replaying history is a read, not a generation request.

Video operations reserve an application operation ID before submission. An
optional media repository supports atomic create and compare-and-swap updates.
Successful submission saves the provider job handle immediately. A lost response
or failed save leaves an ambiguous operation that must not be resubmitted.
Persisted handles resume polling. Terminal state is immutable; concurrent or
stale observations cannot overwrite it. Local cancellation stops waiting and
does not imply remote cancellation or zero billing. Remote cancellation is an
optional provider interface, never simulated by deleting a result.

The initial adapter matrix is OpenAI image generation, file transcription,
live transcription and speech; OpenRouter dedicated image/video/file
transcription/speech endpoints. OpenRouter live audio input streaming is not
claimed. OpenAI Sora is deprecated with shutdown scheduled 2026-09-24 and is not
the baseline video adapter. Models are explicit configuration, with conservative
known profiles and explicit validated capability configuration for new models.
Advanced edits, masks, voice cloning, SSML, video webhooks and transcoding are
outside the initial adapter surface. Unsupported controls return errors.

Streams use synchronous consumer callbacks/readers for backpressure. Audio
chunks form one encoded stream. Live STT commits one utterance at end-of-input;
events replace provisional text by segment ID and finish with finalized text.
No reconnect, audio replay, automatic generation retry or provider fallback.
Polling alone may retry rate limits/transient failures within a bounded budget.

Operation metadata integrates with existing model-attempt repositories using
namespaced metadata. Native units and precise provider-reported decimal USD
amounts are optional; absent cost stays unknown. Full pricing resolver and
aggregation work remains MAD-84. Default diagnostics exclude prompts,
transcripts, media, provider error messages, credentials and signed URLs.

Sources: [OpenRouter images](https://openrouter.ai/docs/guides/overview/multimodal/image-generation),
[video](https://openrouter.ai/docs/guides/overview/multimodal/video-generation),
[STT](https://openrouter.ai/docs/guides/overview/multimodal/stt),
[TTS](https://openrouter.ai/docs/guides/overview/multimodal/tts),
[OpenAI images](https://developers.openai.com/api/docs/guides/image-generation),
[file STT](https://developers.openai.com/api/docs/guides/speech-to-text),
[live STT](https://developers.openai.com/api/docs/guides/realtime-transcription),
[TTS](https://developers.openai.com/api/docs/guides/text-to-speech).
