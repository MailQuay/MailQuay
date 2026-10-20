GO ?= go
SQLC ?= sqlc
OAPI_CODEGEN ?= oapi-codegen
GOLANGCI_LINT ?= golangci-lint
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

.PHONY: all build build-full generate api-lint test test-race test-integration lint migrate-up migrate-down search-reindex clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/mailquay-core ./cmd/mailquay

build-full:
	@test -d internal/webassets/dist || (echo "internal/webassets/dist is missing; build the frontend and copy dist first" >&2; exit 1)
	CGO_ENABLED=0 $(GO) build -trimpath -tags full -ldflags "$(LDFLAGS)" -o bin/mailquay ./cmd/mailquay

generate:
	$(SQLC) generate
	$(OAPI_CODEGEN) -config api/oapi-codegen.yaml api/openapi.yaml

api-lint:
	$(OAPI_CODEGEN) -config api/oapi-codegen.yaml api/openapi.yaml
	git diff --exit-code -- internal/httpapi/oapi/api.gen.go

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-integration:
	$(GO) test -tags=integration ./test/integration/...

lint:
	$(GO) vet ./...
	$(GOLANGCI_LINT) run

migrate-up:
	$(GO) run ./cmd/mailquay migrate up

migrate-down:
	$(GO) run ./cmd/mailquay migrate down 1

search-reindex:
	$(GO) run ./cmd/mailquay search reindex --all

clean:
	rm -rf bin
