# Cloud handoff: decision models

This is the handoff for the team that operates Lebro Cloud. The library
release adds a provider-neutral decision contract and three BYOK adapters. The
Cloud product owns everything around it: tenant membership, encrypted
credential storage and rotation, provider onboarding, audit and retention
experience, and the product policy that decides what a judgment may do.

Nothing in this document is implemented in the library repository, and the
library never requires a hosted Lebro proxy. A Cloud deployment configures
the library's adapters with the tenant's own credential and points them at the
tenant's own provider account.

## What the library provides

| Concern | Library | Cloud |
| --- | --- | --- |
| Request and answer contract | `lebro.DecisionRequest`, `lebro.DecisionResponse` | unchanged |
| Question kinds and validation | `lebro.NoulQuestion`, `lebro.ChoiceQuestion`, `lebro.ScoreQuestion` | unchanged |
| Provider capability description | `lebro.DecisionCapabilities` | the picker reads it |
| Normalized failures | `*lebro.DecisionError` and the `ErrDecision*` sentinels | the UI maps them |
| Provider wire formats | `decisions.NewTypeSafe`, `decisions.NewFastino`, `decisions.NewSubQ`, `decisions.NewOpenAI` | unchanged |
| Credential storage and rotation | none | Cloud |
| Tenant and member authorization | none | Cloud |
| Audit trail | none | Cloud |
| Retention disclosure and enforcement | none | Cloud |
| Thresholds, gates, and actions | the application decides | the tenant's policy |

## Construction and configuration

A Cloud deployment builds one adapter per tenant and provider, and keeps the
adapter for the lifetime of the process. The credential is read from the
tenant's secret store and handed to the adapter; the adapter never persists
it, never logs it, and never returns it. An adapter captures the credential
when it is built, so a rotated credential needs a rebuilt adapter rather than
a new request.

```go
model, err := decisions.NewSubQ(decisions.SubQConfig{
    APIKey:  tenantSecret,
    Model:   "subq-snap-preview",
    Timeout: 30 * time.Second,
})
```

A Cloud deployment must not:

- put a credential in a URL, a log line, a metric label, a trace attribute, or
  an error message;
- send state or images to a provider the tenant has not enabled;
- copy a decision answer into a transcript as if it were a model message; a
  judgment is an input to policy, not a turn in the conversation;
- widen an adapter's image limits to fit a request. The limits are the
  provider's, and a request beyond them fails locally.

## Model and capability picker

The picker should read `lebro.DecisionModel.Capabilities` rather than a
hard-coded list, and it must respect what the tenant enabled:

- `Noul`, `Choice`, and `Score` gate the question kinds a tenant may use.
- `MultipleQuestions` and `MaxQuestions` gate how many questions one request
  may carry.
- `StructuredState` and `StructuredInstructions` gate whether a tenant may
  send structured content.
- `Images.Enabled`, `Images.MediaTypes`, `Images.MaxImages`,
  `Images.MaxBytesPerImage`, and `Images.MaxTotalBytes` gate the image
  inputs a tenant may send.
- `NoulUncertainty`, `ChoiceUncertainty`, and `ScoreUncertainty` gate what a
  UI may promise about an answer. The uncertainty belongs to the question
  kind, not to the provider: a noul answer from TypeSafe and SubQ carries no
  confidence even though the provider reports confidence for its choice and
  score answers. A missing confidence is shown as unknown, never as a low or
  high value, and a UI must not compare two confidences whose
  `ConfidenceMetric` differs.
- `RetryableStatuses` describes the provider's own retry contract. The
  adapter applies it; the Cloud must not add retries of its own on top.
- `CapabilitiesFor(model)` describes one model rather than the adapter as a
  whole. A picker that offers a model the tenant enabled must read the
  profile of that model, and it must not offer a model the provider has no
  verified profile for.

The capability matrix in [decisions.md](decisions.md#capability-matrix) is
the reference for each provider.

## Authorization, audit, and policy

A decision answer is not an authorization. Cloud owns the boundary:

1. The tenant's policy decides which actions a judgment may influence. A
   judgment never authorizes a tool, a spend, or a data access on its own.
2. A guarded action still goes through the existing exact-action approval
   path, so the reviewed payload stays bound to the tool call that was
   reviewed.
3. A denial, an uncertain answer, a timeout, a cancellation, a provider
   failure, and a refusal must all leave the action unauthorized. Treat "no
   answer" as "not authorized", and treat a refusal as "not authorized": a
   provider that declined to answer has not authorized anything.
4. A decision sits before the runtime's own guarantees, so it does not
   weaken them. Transcript integrity, tool-pair integrity, compare-and-swap
   fencing of a run, and the deterministic context budget still apply to a
   run whose handler was guarded by a judgment.
5. The audit trail records that a decision was consulted, which provider and
   model answered, the question identifiers, the outcome, and the attempt
   count. It records the requested model, the resolved model, and the upstream
   provider separately, because a gateway's answer can differ from the model
   the request named. It does not need the state, the criteria, or the images
   to be reproducible, and it should not copy them by default.

`lebro.DecisionGate` produces a recommendation from caller-owned thresholds.
It is useful as the default policy layer, and it deliberately cannot
authorize anything: the tenant's policy still owns the effect.

## Retention and disclosure

A decision request sends the tenant's state, questions, and images to a
third-party provider. Cloud must:

- disclose the provider, the model, and the provider's retention policy
  before the tenant enables it. TypeSafe's retention is not verified in this
  release, so the disclosure has to say so rather than present an unknown
  policy as documented. OpenAI supports Zero Data Retention and HIPAA use for
  eligible customers, and residency in the United States and Europe, but the
  eligibility depends on the account's agreements, so the disclosure has to
  read the tenant's data controls rather than assume them;
- apply the tenant's data policy to the request before it is sent, and refuse
  to send content the tenant's policy excludes;
- record the provider retention consequence in the tenant's own audit
  history, so an operator can answer what a third party holds;
- offer a way to keep a request out of provider storage where the provider
  supports it. Fastino honors `FastinoConfig.DisableStorage`, which sends the
  wire field `store: false`, so the Cloud setting is a constructor option
  rather than a wire field. SubQ keeps nothing under a zero data retention
  organization, and otherwise stores decisions, including their images, under
  the organization's retention policy.

Cloud must never log or persist the state, the criteria, or the image bytes
in order to make a decision provider call. If the tenant needs that record,
it is the tenant's data and it belongs in the tenant's own store under the
tenant's own policy.

## Migration

Existing generative clients are unaffected. The decision surface is additive
and lives behind its own interface, so a tenant can enable one provider, one
question kind, and one guarded action at a time. See
[the migration guide](migrations.md) for the library-side summary.

## BYOK smoke-test checklist

Run this against a tenant's own credential, in a non-production tenant, before
the tenant's traffic is enabled. Record the provider, the model, the observed
latency, the usage, and the observed uncertainty behavior for each run.

- [ ] The credential is read from the secret store and is never written to a
      log, a metric, a trace, or an error. The adapter captures the
      credential when it is built, so Cloud rebuilds the adapter after a
      rotation; the rebuilt adapter takes effect without a code change.
- [ ] A live run records the provider, the model, the model version, the
      modality it exercised, the latency, the usage, and the uncertainty it
      observed, and the recorded result is the one the tenant sees.
- [ ] A noul question returns a probability, and a threshold on that
      probability produces the expected outcome.
- [ ] A choice question returns the selected option and a distribution that
      covers every requested option.
- [ ] A score question returns a weighted level. For Fastino, which reports
      both, the selected level stays separate from the weighted value; for
      TypeSafe and SubQ, which report only the weighted value, the selected
      level is absent and the answer says so.
- [ ] A request with two questions returns one answer per question, keyed by
      the question identifier.
- [ ] A provider that reports no confidence produces a `nil` confidence, and
      the tenant's UI shows the uncertainty rather than implying certainty.
- [ ] A request that exceeds an adapter-advertised limit fails locally,
      before any request reaches the provider. A limit the adapter does not
      model, such as a context window, stays provider-authoritative.
- [ ] An image request with an unsupported media type fails locally, and the
      image bytes appear nowhere in the failure.
- [ ] An image request that carries a URL or a data URL fails locally, and
      the adapter never fetches it.
- [ ] A rate-limited response is retried within the tenant's budget, and the
      `Retry-After` delay is honored.
- [ ] A denied, an uncertain, a timed-out, and a failed decision all leave
      the guarded action unauthorized, and the tool handler does not run.
- [ ] The exact-action approval still binds the reviewed payload after a
      decision has been consulted.
- [ ] The provider's retention behavior matches what the tenant was told, and
      the audit history records the disclosure.
