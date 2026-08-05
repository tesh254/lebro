# Examples

Each directory is a standalone, runnable program for one `lebro` feature set.
Keep examples small, provider-neutral, and focused on public APIs so they act
as executable documentation while the library grows.

Run the JSON Schema validation example from the repository root:

```sh
go run ./examples/schema-validation
```

Run the in-memory storage example:

```sh
go run ./examples/storage-memory
```

As features are added, create sibling directories such as `model-*`,
`tools-*`, or `workflow-*` rather than extending an unrelated example.
