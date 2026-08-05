# Installation

## Install Go

lebro uses Go 1.26.5. Install Go from the [official Go download
page](https://go.dev/dl/), then verify it:

```sh
go version
```

The module includes `toolchain go1.26.5`. With the default
`GOTOOLCHAIN=auto`, an older supported Go command downloads and uses Go 1.26.5
for this module automatically.

## Add lebro to a module

After the first release:

```sh
go get github.com/tesh254/lebro@v0.1.0
```

To opt into the most recent development version before the first tag:

```sh
go get github.com/tesh254/lebro@latest
```

Then import the root package:

```go
import "github.com/tesh254/lebro"
```

## Upgrade

Upgrade to a specific compatible release when it is published:

```sh
go get github.com/tesh254/lebro@v0.1.0
go mod tidy
```

Run your normal test suite after every dependency upgrade.
