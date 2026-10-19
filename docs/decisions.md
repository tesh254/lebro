# Decision models

A decision model evaluates application state against named questions and
returns typed answers: a probability, a selected option, a level, a
distribution. It is a separate contract from
[`lebro.Model`](../model.go), which generates text and tool calls. The two
compose in one application, and adding a decision surface changes nothing
about an existing generative client.

The decision surface is provider-neutral. A provider package translates its
own wire format into the contract, so switching providers changes
configuration, not application logic.

```go
import "github.com/tesh254/lebro/decisions"

model, err := decisions.NewTypeSafe(decisions.TypeSafeConfig{
    APIKey: os.Getenv("TYPESAFE_API_KEY"),
    Model:  "jev-latest",
})
```

## The contract

A request carries the state to evaluate, the questions to ask, and optional
image inputs.

```go
response, err := model.Decide(ctx, lebro.DecisionRequest{
    State: "My card was charged twice for order A-104.",
    Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
        "is_refund": lebro.NoulQuestion{
            Instructions: "Does this request qualify for a refund?",
            Criteria: &lebro.NoulCriteria{
                True:  "Qualifies",
                False: "Does not qualify",
            },
        },
        "department": lebro.ChoiceQuestion{
            Instructions: "Which team should handle this request?",
            Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
                "billing":   "Payment and refund questions",
                "technical": "Product faults and integrations",
            },
        },
        "urgency": lebro.ScoreQuestion{
            Instructions: "How urgent is this request?",
            Criteria:     []lebro.JSONValue{"Can wait", "Handle today"},
        },
    },
})
```

Three question kinds are available, and the set is closed:

| Question | Criteria | Answer |
| --- | --- | --- |
| `NoulQuestion` | Optional `True` and `False` descriptions | The probability that the condition holds |
| `ChoiceQuestion` | A map of caller-owned option IDs to descriptions | The selected option and the distribution over every option |
| `ScoreQuestion` | An ordered list of level descriptions, lowest first | The weighted level, and a winning level when the provider reports one |

A response carries one answer per requested question, keyed by the
caller-owned question ID, plus the provider and model identity, the reported
usage, the provider request identifier, the elapsed time, and the number of
attempts.

```go
answer := response.Answers["urgency"].(lebro.ScoreAnswer)
*answer.ExpectedLevel  // the probability-weighted level
*answer.SelectedLevel  // the winning level, or nil when the provider reports none
```

## Provider semantics are preserved

The contract keeps apart three things a decision provider can report, and it
never fills in a value the provider did not report.

- **A binary probability is not an action.** `NoulAnswer.ProbabilityYes` is a
  probability from zero to one. The application owns the threshold, and
  `lebro.DecisionGate` turns an answer into a recommendation
  (`select`, `review`, or `escalate`) that the application still owns.
- **A selected level and a weighted level are different.** `SelectedLevel` is
  the level the provider itself picked, and `ExpectedLevel` is the
  probability-weighted value. TypeSafe and SubQ report only the weighted
  value, so their `SelectedLevel` stays `nil`; Fastino reports both.
- **Confidence and distributions are reported, never derived.**
  `Confidence` is `nil` when the provider does not report one, and a
  distribution must cover every requested option or level. A response that
  cannot be interpreted is a `lebro.ErrDecisionMalformedResponse` failure, not
  a partially filled answer.

`ScoreAnswer.Provenance` records where a value came from. `ScoreNative` means
the provider reported it; `ScoreNormalized` means an adapter derived it from
a documented distribution. The adapters in this release report native values
only.

## Capability matrix

`lebro.DecisionModel.Capabilities` reports what a provider supports, so an
application can pick one before it sends anything. A zero limit means the
provider documents no limit, not that the limit is zero.

| Capability | TypeSafe (Jev) | Fastino (GLiDE) | SubQ (Snap preview) |
| --- | --- | --- | --- |
| Endpoint | `POST https://api.typesafe.ai/v1/systemone` | `POST https://api.fastino.ai/v1/systemone` | `POST https://api.subq.ai/v1/systemone` |
| Model | `jev-latest` (the response echoes the concrete model) | `fastino/GLiDE` (the response echoes `glide`) | `subq-snap-preview` |
| Authentication | `Authorization: Bearer` | `X-API-Key` | `Authorization: Bearer` |
| Noul, Choice, Score | yes | yes | yes |
| Multiple questions | yes | yes | yes, up to 16 |
| Structured state | yes | yes | yes |
| Structured instructions | yes | no, text only | yes |
| Distributions | yes | yes | yes |
| Confidence | choice and score only | every answer type | choice and score only |
| Image media types | not exposed by this adapter | not exposed by this adapter | `image/png`, `image/jpeg`, `image/webp` |
| Maximum images | not exposed by this adapter | not exposed by this adapter | 4 |
| Maximum bytes per image | not exposed by this adapter | not exposed by this adapter | 4 MiB decoded |
| Maximum total image bytes | not exposed by this adapter | not exposed by this adapter | 8 MiB decoded |
| Maximum choice options | 255 | 255 | 255 |
| Maximum score levels | 10 | 255 | 10 |
| Request body limit | 32 MiB | not documented as a byte limit | 13 MiB |
| Context window | not documented | about 40,000 tokens per rendered question | 384K tokens (393,216) |
| Batching | no | no | no |
| Retryable statuses | 429, 529 | 425, 429, 503 | 429, 503 |
| Score answer | weighted value, no winning level | winning level and weighted value | weighted value, no winning level |
| Request identifier | `x-typesafe-request-id` | not documented | not documented |

TypeSafe documents an image capability of its own. This release does not expose
it, so the adapter advertises no image support rather than guessing at a wire
format the release does not implement.

A model identifier is configuration, not a library assumption: each adapter
sends the model from its configuration, or the model a request names, and
never rewrites it. The defaults in the matrix are the identifiers the
providers document today, and `Config.Model` overrides them for an account
that uses a different alias or a pinned version.

### The generic System One adapter

The three named adapters are the generic one, configured. Everything that
differs between the providers is data in `SystemOneConfig`, so a provider
this package ships no named adapter for is reachable by configuration alone:

```go
model, err := decisions.NewSystemOne(decisions.SystemOneConfig{
    APIKey:     os.Getenv("SOME_SYSTEM_ONE_API_KEY"),
    BaseURL:    "https://api.example.com",
    AuthHeader: "X-API-Key",
    Model:      "some-model",
    Score:      decisions.SystemOneScoreFields{Weighted: "score"},
    Capabilities: lebro.DecisionCapabilities{
        Noul: true, Choice: true, Score: true,
        MultipleQuestions: true, StructuredState: true,
        StructuredInstructions: true,
        Distributions: true, Confidence: true,
        MaxQuestions: 16, MaxChoiceOptions: 255, MaxScoreLevels: 10,
    },
    RetryableStatuses: []int{429, 503},
})
```

The score field names are the part worth reading twice. A provider reports
the probability-weighted level in one field, and some report the winning
level in another, so the names belong to the provider:

| Provider | `SystemOneScoreFields` | Effect |
| --- | --- | --- |
| TypeSafe, SubQ | `{Weighted: "score"}` | `ExpectedLevel` is set, `SelectedLevel` stays nil |
| Fastino | `{Weighted: "expected_level", Winner: "score"}` | `ExpectedLevel` is the weighted value, `SelectedLevel` is the winner |

A configuration whose weighted and winner fields are the same is rejected
when the adapter is built. A provider that reports no field the
configuration names is a malformed response, not a silently different
answer.

The safe defaults are the restrictive ones: instructions and criteria are
text only, image inputs are disabled, and the provider is unnamed. Set
`Capabilities.StructuredInstructions` for a provider that accepts
structured values.

### Image inputs are per provider

The image wire shape is the one part of the protocol the providers do not
share:

| Provider | Shape |
| --- | --- |
| SubQ | a top-level `images` array of `{mediaType, base64}`, alongside the state |
| TypeSafe | a top-level `assets` map, with references written into the state, instructions, and criteria, and a `TypeSafe-Danger-Zone-Images: true` header |
| Fastino | `image_url` parts inside the state, carrying URLs |

So the image style is selected per provider. `SystemOneImagesInline` is the
shape this package implements, and a provider configured with a shape it does
not implement advertises no image support at all: a request that carries an
image fails locally with `lebro.ErrDecisionUnsupportedCapability` rather
than reaching the provider.

### Running the same adapter through a gateway

A gateway that fronts a System One provider speaks the same protocol, so the
adapter needs a different endpoint, credential, and model alias and nothing
else. The application code, the questions, and the answers stay identical:

```go
model, err := decisions.NewTypeSafe(decisions.TypeSafeConfig{
    APIKey:  os.Getenv("OPENROUTER_API_KEY"),
    BaseURL: "https://openrouter.ai/api",
    Model:   "~typesafe/jev-latest",
})
```

The request reaches the provider through the gateway, and the answers keep
the provider's semantics: a noul answer from Jev still carries no
confidence, and a score answer still carries no native winner. The gateway
reports the request identity in its own response header, which the adapter
reads alongside the provider's.

A gateway's System One endpoint takes the provider's own model alias, not the
gateway's chat model of the same family. On OpenRouter that alias is
`~typesafe/jev-latest`; `typesafe/jev-router` is a chat model that routes
between Jev variants, and the System One endpoint rejects it.

Fastino accepts text instructions and text criteria only. A request that
carries structured instructions or criteria fails with
`lebro.ErrDecisionInvalidRequest` before anything is sent, because the
provider does not describe a structured form for them.

## Image inputs

Image input is an optional, capability-gated part of the contract. A
`lebro.DecisionImage` carries a media type and raw bytes. It holds image
data, not a reference to it: an adapter never fetches an image, and a URL or
a data URL is rejected locally because it is not image data. A file path is
not image data either, and it is rejected locally too, because the bytes do
not match the declared media type.

An adapter rejects a non-empty `Images` slice before it makes an HTTP request
unless its advertised `DecisionCapabilities.Images` allows the media type and
the limits. A provider with no verified image support advertises none, so an
image request fails with `lebro.ErrDecisionUnsupportedCapability` instead of
silently degrading to a text-only request.

A URL, a data URL, and a remote reference are not image data. They fail
locally as an invalid request, because an adapter never fetches an image and
a reference must never reach the provider as an opaque payload.

The SubQ adapter encodes image bytes as one line of base64 while it serializes
its HTTP request, and nowhere else. The bytes are not retained, not logged,
and not included in an error message.

This release adds no image processing: no downsampling, no conversion, no
OCR, no layout analysis, and no video. Callers provide already-authorized,
correctly sized images.

## BYOK and redaction

Every adapter is configured by the caller:

```go
decisions.NewFastino(decisions.FastinoConfig{
    APIKey:       os.Getenv("FASTINO_API_KEY"),
    BaseURL:      "https://api.fastino.ai",  // optional
    Model:        "fastino/GLiDE",           // optional
    HTTPClient:   nil,                       // optional
    Timeout:      0,                         // optional, 60s by default
    MaxRetries:   nil,                       // optional, 2 by default
    RetryBackoff: 0,                         // optional, 500ms by default
})
```

A credential is sent in a header and is never logged, returned, persisted, or
included in an error message. A provider error body is never echoed verbatim:
only its documented message fields are read, and only within a bounded length,
so a validation error cannot carry request content such as an image back into
an error string.

## Failures

Failures are normalized into `*lebro.DecisionError`, which carries a kind, the
provider, the provider error code, the HTTP status, and the `Retry-After`
delay when the provider reported one. `errors.Is` matches the kind sentinels:

| Kind | Sentinel | Retried |
| --- | --- | --- |
| `invalid_request` | `lebro.ErrDecisionInvalidRequest` | no |
| `unsupported_capability` | `lebro.ErrDecisionUnsupportedCapability` | no |
| `authentication` | `lebro.ErrDecisionAuthentication` | no |
| `permission_denied` | `lebro.ErrDecisionPermissionDenied` | no |
| `rate_limited` | `lebro.ErrDecisionRateLimited` | yes |
| `unavailable` | `lebro.ErrDecisionUnavailable` | yes |
| `timeout` | `lebro.ErrDecisionTimeout` | yes |
| `transport` | `lebro.ErrDecisionTransport` | yes |
| `cancelled` | `lebro.ErrDecisionCancelled` | no |
| `malformed_response` | `lebro.ErrDecisionMalformedResponse` | no |

`errors.Is(err, context.Canceled)` and `errors.Is(err, context.DeadlineExceeded)`
keep working, so a caller can still tell a cancelled request from a failed
one.

A retry is bounded by `MaxRetries` and backs off exponentially. A
`Retry-After` delay replaces the backoff when the provider asks for a longer
one. A decision request is a read-only judgment, so retrying it is safe, but
an adapter never switches to another provider: a failed decision stays failed.

## Provider retention

A decision request sends the caller's state, questions, and images to a
third-party provider. That is the point of BYOK, and it is also a disclosure
an application has to make to its own users.

- **SubQ** stores decisions, including their images, under the
  organization's retention policy. A zero data retention organization keeps
  nothing.
- **Fastino** stores the request and the answer by default. Set
  `FastinoConfig.DisableStorage` to send `store: false` and keep the payload
  out of storage, logs, error reports, and traces. The response carries the
  applied policy in the `X-Fastino-Data-Retention` header.
- **TypeSafe** retention is not verified in this release. Follow the
  provider's own documentation.

See the [Cloud handoff](decisions-cloud-handoff.md) for how a hosted
deployment is expected to handle this.

## OpenAI

OpenAI's native Decisions API is **not** implemented in this release, and the
`openai` package remains a chat-completions adapter. The OpenAI Decisions
surface differs from the System One protocol these adapters share: its
questions and answers are arrays rather than maps, its binary question is a
`predicate` rather than a noul, it can answer with a refusal, and its input
takes user messages rather than a shared state. An adapter for it needs its
own wire translation, a refusal answer in the public contract, and its own
contract suite. That work is tracked separately and does not block the three
working adapters.

## Examples

- [`decision-routing`](../examples/decision-routing) routes a request, gates
  an action on a confidence threshold, and combines score answers with
  application-owned weights.
- [`decision-guard`](../examples/decision-guard) puts a decision in front of
  a dangerous tool and shows that a denial, an uncertain answer, a provider
  failure, and a tool outside the allowlist all leave the handler uncalled.
- [`decision-images`](../examples/decision-images) sends a bounded image to
  the SubQ adapter and escalates an uncertain answer.
- [`decision-compat`](../examples/decision-compat) shows a generative client
  that needed no change.

## Live tests

The fixture suite is the regression gate. A live run against a real provider
is a separate, opt-in check that needs a credential:

```sh
LEBRO_TYPESAFE_API_KEY=... go test ./decisions/ -run TestLiveTypeSafe -v
LEBRO_FASTINO_API_KEY=... go test ./decisions/ -run TestLiveFastino -v
LEBRO_SUBQ_API_KEY=... go test ./decisions/ -run TestLiveSubQ -v
LEBRO_OPENROUTER_API_KEY=... go test ./decisions/ -run TestLiveTypeSafeViaOpenRouter -v
```

Each run reports the provider, the model the provider echoed, the latency,
the usage, the modality it exercised, and the uncertainty it observed. A run
without the credential is skipped, so the default suite stays offline.
