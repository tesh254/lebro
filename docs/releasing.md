# Releasing lebro

This project is a Go library, so a release is a semantic Git tag and GitHub
Release rather than a compiled application artifact.

## Prerequisites

- Changes are merged into `main`.
- CI is green.
- `CHANGELOG.md` describes the release.
- Your local branch is up to date with `origin/main`.

## Publish the first release

```sh
git checkout main
git pull --ff-only
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Pushing a `v*` tag runs the release workflow. It verifies the Go module and
creates GitHub release notes. Go users can then install the version with:

```sh
go get github.com/tesh254/lebro@v0.1.0
```

## Subsequent releases

Use semantic versioning. A patch release (`v0.1.1`) is source- and wire-
compatible. Before `v1.0.0`, additive and breaking public changes both require
a new minor version (`v0.2.0`); never place a breaking change in a patch.

At `v1.0.0` and later, additive public changes use a new minor version and a
breaking exported Go API or supported wire-contract change requires a new major
version and matching module path, such as `github.com/tesh254/lebro/v2`.

Follow [the stability policy](stability.md), add upgrade notes to
`CHANGELOG.md`, and update [the migration guide](migrations.md) for every
persistence or wire-contract change.
