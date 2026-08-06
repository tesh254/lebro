GOLANGCI_LINT_VERSION ?= v1.64.8

.PHONY: test vet lint lint-install check

test:
	go test ./...

vet:
	go vet ./...

lint-install:
	GOBIN=$(CURDIR)/bin go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint:
	./bin/golangci-lint run ./...

check: test vet lint
