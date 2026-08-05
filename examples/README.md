# Examples

Each directory is a standalone, runnable program for one `lebro` feature set.
Keep examples small, provider-neutral, and focused on one public API or
repository test harness so they act as executable documentation while the
library grows.

Run the JSON Schema validation example from the repository root:

```sh
go run ./examples/schema-validation
```

Run the in-memory storage example:

```sh
go run ./examples/storage-memory
```

Run the deterministic, network-free model fixture example:

```sh
go run ./examples/model-fixtures
```

This example imports `internal/testkit`, which is deliberately available only
to this repository's tests and examples. Production packages do not depend on
the fixture harness.

As features are added, create sibling directories such as `model-*`,
`tools-*`, or `workflow-*` rather than extending an unrelated example.
