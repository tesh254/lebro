# OTLP observability

This example exports one filtered Lebro span through `obsv/otlp`. It defaults to an in-process collector:

```sh
go run ./examples/observability-otlp
```

Set `OTLP_PROVIDER` to send one real trace. Keep credentials in environment variables.

## Axiom

```sh
OTLP_PROVIDER=axiom AXIOM_DOMAIN=YOUR_AXIOM_DOMAIN AXIOM_TOKEN=YOUR_API_TOKEN AXIOM_DATASET=YOUR_TRACE_DATASET go run ./examples/observability-otlp
```

Uses `/v1/traces`, `Authorization: Bearer`, and `X-Axiom-Dataset`.

## Datadog

```sh
OTLP_PROVIDER=datadog DATADOG_OTLP_TRACES_ENDPOINT=YOUR_DATADOG_OTLP_TRACES_ENDPOINT DD_API_KEY=YOUR_API_KEY go run ./examples/observability-otlp
```

Endpoint varies by Datadog site/deployment. Uses OTLP/HTTP protobuf, `dd-api-key`, and `compute_stats`.

## Langfuse

```sh
OTLP_PROVIDER=langfuse LANGFUSE_PUBLIC_KEY=pk-lf-... LANGFUSE_SECRET_KEY=sk-lf-... LANGFUSE_BASE_URL=https://cloud.langfuse.com go run ./examples/observability-otlp
```

Uses Basic Auth and `x-langfuse-ingestion-version: 4` for immediate v4 visibility. Set `LANGFUSE_BASE_URL` for US, Japan, HIPAA, or self-hosted Langfuse.

## LangSmith

```sh
OTLP_PROVIDER=langsmith LANGSMITH_API_KEY=YOUR_API_KEY LANGSMITH_PROJECT=support-agent go run ./examples/observability-otlp
```

Default endpoint is `https://api.smith.langchain.com/otel/v1/traces`. Set `LANGSMITH_OTLP_TRACES_ENDPOINT` for regional or self-hosted LangSmith.
