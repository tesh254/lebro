# Examples

Each directory is a standalone, runnable program for one `lebro` feature set.
Keep examples small, provider-neutral, and focused on public APIs so they act
as executable documentation while the library grows.

Run an example from the repository root:

```sh
go run ./examples/storage-memory
```

As features are added, create sibling directories such as `model-*`,
`tools-*`, or `workflow-*` rather than extending an unrelated example.
