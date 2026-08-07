# Contributing to lebro

## Prerequisites

- Go 1.26.5 or newer
- Git

Go automatically selects the module's pinned toolchain when `GOTOOLCHAIN=auto`
(the default).

## Local setup

```sh
git clone https://github.com/tesh254/lebro.git
cd lebro
go test ./...
go vet ./...
```

Keep packages provider- and storage-neutral unless the package is explicitly an
adapter. Keep root `lebro` as stable public API façade; place runtime
implementation in `internal/runtime` and optional integrations in their own
packages. Every public-contract change needs tests and documentation.

## Before opening a pull request

```sh
go test ./...
go vet ./...
```

Use focused commits, explain public API changes in the pull request, and avoid
introducing a concrete provider dependency into the root module.
