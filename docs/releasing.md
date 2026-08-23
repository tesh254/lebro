# Releasing lebro

This project is a Go library, so a release is a semantic Git tag and GitHub
Release rather than a compiled application artifact.

## Prerequisites

- Changes are merged into `main`.
- CI is green.
- `CHANGELOG.md` describes the release.
- Your local branch is up to date with `origin/main`.

## Publish a release

Merge a pull request into `main` with exactly one release label:

| Label | Next version |
| --- | --- |
| `release:patch` | increments patch (`v0.1.0` to `v0.1.1`) |
| `release:minor` | increments minor and resets patch (`v0.1.0` to `v0.2.0`) |
| `release:major` | increments major and resets minor/patch (`v0.2.0` to `v1.0.0`) |

GitHub Actions calculates the next SemVer tag from existing release tags,
verifies tests, vet, lint, and the statement-coverage ratchet
(`scripts/check-coverage.sh`: coverage may not drop below the recorded
baseline in `scripts/coverage-baseline`), then creates the tag and GitHub
release from the merge commit. A missing or duplicate release label fails the
release job without creating a tag.

After the first release is indexed, Go users can install it with:

```sh
go get github.com/tesh254/lebro@v0.1.0
```

### Publishing to pkg.go.dev

`pkg.go.dev` indexes a module automatically after its first semantic version
tag is pushed. The module already meets every indexing requirement: the module
path is `github.com/tesh254/lebro`, the repository carries an Apache-2.0
`LICENSE`, `go.mod` is well-formed, and the root package has a package-level
doc comment.

After the release workflow creates the tag:

1. Wait a few minutes; the indexer usually picks the tag up on its own.
2. Check `https://pkg.go.dev/github.com/tesh254/lebro` (and any optional
   packages you link, such as `httpapi` or `evals`).
3. If nothing appears within an hour, request indexing once at
   `https://pkg.go.dev/request` with the module path `github.com/tesh254/lebro`
   — no login required, and later tags re-index automatically.

Once indexed, update relative source links in `README.md` to
`pkg.go.dev/github.com/tesh254/lebro/...` links so readers land on rendered,
versioned documentation. A README badge is optional; if you add one, point it
at the latest indexed version rather than `@latest` so it survives pre-v1
breaking changes.

## Subsequent releases

Use semantic versioning. A patch release (`v0.1.1`) is source- and wire-
compatible. Before `v1.0.0`, additive and breaking public changes both require
a new minor version (`v0.2.0`); never place a breaking change in a patch.

At `v1.0.0` and later, additive public changes use a new minor version and a
breaking exported Go API or supported wire-contract change requires a new major
version and matching module path, such as `github.com/tesh254/lebro/v2`.

Before `v1.0.0`, use `release:minor` for breaking stable changes. Reserve
`release:major` for the intentional `v1.0.0` promotion; after that it creates
the next Go module major version. Follow [the stability policy](stability.md), add upgrade notes to
`CHANGELOG.md`, and update [the migration guide](migrations.md) for every
persistence or wire-contract change.
