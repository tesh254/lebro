# CI-gated agent releases

A Braintrust/LangSmith-style release gate: a prompt or model change runs
against a versioned dataset, and the deploy is blocked when real cases
regress. Dataset versions are content hashes, so comparisons stay honest — a
delta always describes the same questions.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| Honest comparisons | `evals.Dataset.Version()` — a content hash over the ordered cases; `Compare` refuses records from different versions |
| Real target | `evals.NewAgentTarget` wrapping an actual `*lebro.Agent` (fixture models here; swap in providers unchanged) |
| Per-case scoring | `evals.NewExactMatch` (add `Regexp` / `ModelScorer` as needed) |
| Naming regressions | `evals.Compare` lists the exact `case/scorer` pairs whose pass state moved — aggregate means alone hide them |
| The gate | `comparison.Regressed()` decides `DEPLOY BLOCKED` vs approved |

## Run

```sh
go run ./examples/ci-gated-releases
```

No network or API key required.

## What you should see

- The dataset's content-hash version.
- Baseline: 3/3 passed. Candidate (a prompt rewrite): 2/3 passed.
- `REGRESSED password-reset/exact_match: expected ..., got ...`
- `DEPLOY BLOCKED: regressions against dataset support-bot-regression`

## Wire it into CI

1. Persist experiments to a shared repository (`evals.NewMemoryRepository`
   here; implement `evals.Repository` for your DB) so any later run can be
   compared against the last green build.
2. In CI, run this program with the candidate build; exit non-zero instead of
   printing `DEPLOY BLOCKED`.

See `examples/evals-dataset` for model-graded scorers and offsetting
per-case changes that stable means hide.
