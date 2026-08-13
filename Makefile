GOLANGCI_LINT_VERSION ?= v2.12.2

# LEBRO_STUDIO_UI points at a checkout of the lebro-studio project, whose built
# assets are embedded by the studio package. Override it to build from a
# non-default location: make studio-ui LEBRO_STUDIO_UI=/path/to/lebro-studio
LEBRO_STUDIO_UI ?= ../lebro-studio

.PHONY: test vet lint lint-install check studio-ui

test:
	go test ./...

vet:
	go vet ./...

lint-install:
	GOBIN=$(CURDIR)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: lint-install
	./bin/golangci-lint run ./...

check: test vet lint

# studio-ui builds the lebro-studio UI and copies its client bundle into
# studio/dist so the studio package embeds a usable UI. Run this before packaging
# a release; without it, a released studio serves only the placeholder page. The
# build is intentionally not part of `check`: it needs Node and a lebro-studio
# checkout, which the Go test suite does not.
studio-ui:
	@test -d "$(LEBRO_STUDIO_UI)" || { echo "lebro-studio checkout not found at $(LEBRO_STUDIO_UI); set LEBRO_STUDIO_UI=/path/to/lebro-studio"; exit 1; }
	cd "$(LEBRO_STUDIO_UI)" && pnpm install --frozen-lockfile && pnpm build
	rm -rf studio/dist
	mkdir -p studio/dist
	cp -R "$(LEBRO_STUDIO_UI)/dist/client/." studio/dist/
	@echo "studio/dist populated from $(LEBRO_STUDIO_UI)"
