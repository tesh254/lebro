# Morning competitive-intel digest

A daily-brief bot: at 06:00 a persisted cron schedule fans a job out across
three sources in parallel — news, changelog, pricing — and joins the results
into one brief. No warm process is required: the schedule lives in the store,
so an overnight outage just means the next process to start fires what is due.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| 06:00 cron | `Scheduler` + `ScheduleRecord` persisted to SQLite (`Spec: "0 6 * * *"`, `ConcurrencySkip`) |
| Parallel fan-out | `StepDefinition.FanOut` with three branches and `MaxParallel: 3` |
| Deterministic join | Branch outputs join in declaration order; the join step renders the brief |
| Survives outages | The example closes the store, reopens it as a fresh process, and ticks — the overdue fire runs immediately, and `NextFireAt` advances |

## Run

```sh
go run ./examples/morning-digest
```

No network or API key required; source branches are deterministic stand-ins
for real fetchers. A fixed clock keeps the timing exact.

## What you should see

- The schedule persists, then the "process exits".
- After reopen, one tick fires exactly one overdue schedule.
- The joined brief lists items under news / changelog / pricing.
- The next fire advanced to tomorrow 06:00.

## Swap in production pieces

- Replace each branch's handler with a real fetcher (HTTP scrape, RSS, API).
- Add an agent step after the join to summarize; see
  `examples/workflow-agents-tools`.
- Run the scheduler behind `scheduler.Start(ctx)` instead of manual ticks; see
  `docs/stability.md` for concurrency notes.
