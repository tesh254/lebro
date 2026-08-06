GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: test vet lint lint-install check

test:
	go test ./...

vet:
	go vet ./...

lint-install:
	GOBIN=$(CURDIR)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: lint-install
	./bin/golangci-lint run ./...

check: test vet lint
