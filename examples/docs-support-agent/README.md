# Docs support agent

An Intercom-Fin-style support bot: it answers customer questions **only** from
the public handbook, and every customer keeps one durable, persisted
conversation.

## What it composes

| Concern | lebro primitive |
| --- | --- |
| Handbook search | RAG indexer + vector retriever (`NewIndexer`, `NewVectorRetriever`) |
| Fixed scope | `NewRetrievalTool` with a code-owned metadata filter — no model input can widen the corpus past `visibility:"public"` |
| On-corpus answers | The scripted model refuses unless retrieval returns a relevant chunk; a real model gets the same transcript and the same contract |
| Per-customer memory | `AgentConfig.Store` + `RunInput.ThreadID` — each thread auto-creates on first run and persists every turn |

## Run

```sh
go run ./examples/docs-support-agent
```

No network or API key is required: the embedder, reranker, and model are
deterministic local stand-ins.

## What you should see

- Three handbook documents indexed (one internal).
- Two customers served on separate threads:
  - `customer-acme-1` asks about refunds, then delivery — answers quote the
    matching policy source.
  - `customer-globex-9` asks about internal margin targets — the internal
    document is unreachable through the fixed filter, nothing relevant is
    public, so the agent declines instead of guessing.
- Persisted message counts per thread (`8` for the two-turn customer,
  `4` for the single-turn one), proving transcripts stay isolated per customer.

## Swap in production pieces

- `localEmbedder` → `openai.NewEmbedder` (or any `lebro.EmbeddingModel`).
- `keywordScorer` → any `lebro.RelevanceScorer`.
- `scriptedModel` → `openai.New` / `anthropic.New`; the tool loop, filters,
  threads, and storage are unchanged.

For platform inbound/outbound messaging (Slack-style), see
`examples/channels`; for recalling older history semantically, see
`examples/thread-history`.
