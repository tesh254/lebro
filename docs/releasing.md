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

Use semantic versioning. Additive changes before 1.0 use a new minor version;
breaking public API changes use the next major version and a matching Go module
path such as `github.com/tesh254/lebro/v2`.
