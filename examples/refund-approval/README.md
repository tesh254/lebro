# Refund copilot with human sign-off

A Stripe-dispute-style flow: the agent proposes, a human approves, and the run
survives the wait by parking in a durable suspended state. The refund commits
only after someone holding `refunds:approve` resumes it.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| Propose, don't act | `LinearWorkflow` steps: `propose-refund` → `await-approval` → `commit-refund` |
| Park for a human | `*lebro.SuspendError` + `StepDefinition.SuspendSchema` — the resume consent is schema-checked (`approved` must be exactly `true`, approver non-empty) |
| Survive the wait | SQLite-backed durable snapshots; the example closes and reopens the store to prove the suspension outlives the process |
| Human sign-off | An application-side gate refuses any identity without the `refunds:approve` capability before `Resume` is called |
| Loud rejection | A malformed consent returns `lebro.ErrInvalidResumeInput` without corrupting the snapshot |

## Run

```sh
go run ./examples/refund-approval
```

No network or API key is required.

## What you should see

- The run suspends at `await-approval` with its resume contract printed.
- A support agent without `refunds:approve` is refused; the parked run is
  untouched.
- A malformed consent (`"approved":false`) is rejected by the schema.
- After the simulated restart, an approver with the capability resumes the run;
  the final output shows the committed refund and the approving subject.

## Swap in production pieces

- Wire the capability check into your HTTP handler's authentication instead of
  a helper function.
- Point `commit-refund` at your payment provider — it only executes after a
  valid human approval.
