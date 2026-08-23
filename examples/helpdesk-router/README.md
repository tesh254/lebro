# Helpdesk front desk

A tier-1 triage bot: one entry point receives every employee request, a
deterministic router hands it to the right specialist — IT, HR, or facilities —
under bounded traversal, and every handoff is persisted as an auditable route
record.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| One entry point | `lebro.NewNetwork` with a `WorkflowDefinition` for the whole desk |
| Deterministic routing | `NewRuleRouter` with ordered keyword rules and a default specialist |
| Specialists | Three `*lebro.Agent`s registered as `NetworkSpecialist`s |
| Bounded traversal | `MaxHops` caps handoffs; a specialist is never revisited within one run |
| Audit trail | `NetworkRouteRecord`s persisted into the run record's `StepOutputs`, readable from any `Store` |

## Run

```sh
go run ./examples/helpdesk-router
```

No network or API key required; specialists use deterministic fixture models.

## What you should see

Three tickets, each routed to exactly one specialist:

- VPN/login ticket → `it`
- Leave-policy ticket → `hr`
- Broken-chair ticket → `facilities`

Each line shows the hop count and the specialist's reply; the route records are
decoded back out of the store to prove the traversal is auditable.

## Swap in production pieces

- Replace keyword rules with `NewModelSpecialistRouter` to let a model pick the
  specialist (still validated locally against configured candidates).
- Give each specialist real tools; see `examples/tools-schema`.
