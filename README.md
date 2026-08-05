# lebro

`lebro` is a Go library for composing AI agents, tools, workflows, and their
durable runtime state.

The first release establishes stable public contracts. Provider adapters, tool
execution, the agent loop, and workflow execution arrive in the following
incremental releases. This keeps each layer independently testable and avoids
locking users into a model provider or storage backend.

## Requirements

- Go 1.26.5 or newer

The module pins Go 1.26.5 with Go's `toolchain` directive. With the default
`GOTOOLCHAIN=auto`, Go downloads that toolchain automatically when needed.

## Install

After the first release tag is published:

```sh
go get github.com/tesh254/lebro@v0.1.0
```

For the latest development version before the first tag:

```sh
go get github.com/tesh254/lebro@latest
```

See [the installation guide](docs/installation.md) for toolchain setup and
upgrade instructions.

## Current API foundation

The module already exposes provider-neutral contracts for messages, model
adapters, tools, workflows, and storage. This example validates a canonical
message:

```go
package main

import (
	"fmt"

	"github.com/tesh254/lebro"
)

func main() {
	message := lebro.Message{Role: lebro.RoleUser, Content: "Hello"}
	if err := message.Validate(); err != nil {
		panic(err)
	}

	fmt.Println("message is valid")
}
```

## Examples

Runnable examples live in [examples](examples/README.md), one directory per
feature set. The schema-validation example validates both tool input and output:

```sh
go run ./examples/schema-validation
```

The storage example exercises the repository contracts and in-memory adapter:

```sh
go run ./examples/storage-memory
```

## Development

```sh
git clone https://github.com/tesh254/lebro.git
cd lebro
go test ./...
go vet ./...
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a change. Maintainers
release tagged versions using [the release guide](docs/releasing.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
