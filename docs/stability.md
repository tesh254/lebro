# Stability policy

This policy defines compatibility promises for `github.com/tesh254/lebro`.
It applies to released versions, not an arbitrary commit selected with
`@latest`.

## Status labels

| Status | Meaning |
| --- | --- |
| Stable | Supported public contract. Changes follow the release rules below. |
| Experimental | Public for feedback, but may change in a minor release. It is marked `Experimental` in GoDoc. |
| Internal | `internal/` packages and unexported identifiers. No compatibility promise. |

An optional package is not automatically experimental: optional means root
applications do not import it. Its GoDoc and this policy determine maturity.
Until an API is marked experimental, exported symbols in released root and
optional public packages are stable.

## Go API and semantic versions

Lebro follows semantic versioning with an intentionally strict pre-v1 policy:

- Patch releases never remove, rename, or change behavior of a stable exported
  API, persisted record, or supported wire field incompatibly.
- Before `v1.0.0`, any breaking stable change requires the next minor version;
  additive stable changes also use a minor version. `v0.1.x` users can upgrade
  patch releases without source changes.
- From `v1.0.0`, stable additions use a minor version. Breaking stable Go API
  or supported wire changes require a new major version and Go module path.
- Deprecated symbols remain available through the next minor release before
  removal. GoDoc names the replacement and planned removal release.

`go.mod` dependency updates, provider behavior, and default limits are called
out in release notes when they can change operational behavior.

## What remains compatible

Stable contracts include exported root and public-package types, constructors,
documented errors, JSON fields described by `httpapi` OpenAPI, and persisted
storage records. Additive JSON fields are allowed; clients must ignore unknown
fields. Consumers must not rely on map iteration order, unexported storage
tables, implementation types, timing, or exact error text.

`httpapi.ContractVersion` is independently versioned. Its major version is the
compatibility boundary; `Client.CheckCompatibility` rejects a server with a
different major version. MCP follows its declared protocol version, rather than
the HTTP contract version.

## Promotion checklist

Before removing an `Experimental` marker, maintainers require:

- focused runnable example and package GoDoc;
- validation and error behavior at every public boundary;
- cancellation, concurrency, security, and cost limits documented;
- Memory, SQLite, and Postgres contract coverage when state persists;
- upgrade and wire notes when storage or clients can observe the feature.
