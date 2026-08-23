# Voice booking line

A reservations line: spoken turns transcribe in, run the agent, and synthesize
a reply back out — with booking context kept per caller across calls.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| Speech in / speech out | `voice.Session.Turn`: recognize → agent run → synthesize in one call |
| Per-caller memory | `TurnInput.ThreadID` + `AgentConfig.Store` — every turn persists to the caller's durable thread, and later turns load it |
| Provider swap | Recognizer/synthesizer are interfaces; this example ships fakes |

## Run

```sh
go run ./examples/voice-booking
```

No network, API key, or speech backend required: the recognizer transcribes by
joining audio bytes as text, and the synthesizer echoes the reply as one chunk.
See `examples/voice` for the same stand-ins annotated in full.

## What you should see

- Caller `555` books a Friday table; the line confirms.
- The same caller calls back ("add a high chair") and the agent answers about
  the existing booking — proof the thread carried context across calls.
- A different caller asks about outdoor seating and gets a fresh answer.
- Persisted turn counts per caller (`4` vs `2`) show transcripts stay isolated
  per thread.

## Swap in production pieces

- `fakeRecognizer`/`fakeSynthesizer` → real provider adapters implementing the
  same small interfaces; the session wiring is unchanged.
- Give the host agent a booking tool so replies reflect real inventory; see
  `examples/tools-schema`.
