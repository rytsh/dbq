BINARY_NAME := dbq
BINARY_PATH := ./cmd/${BINARY_NAME}/main.go

BUILD_DATE := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
BUILD_COMMIT := $(shell git rev-parse --short HEAD)
VERSION := $(or $(IMAGE_TAG),$(shell git describe --tags --first-parent --match "v*" 2> /dev/null || echo v0.0.0))

.DEFAULT_GOAL := help

CGO_TAGS := odbc,duckdb
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(BUILD_COMMIT) -X main.date=$(BUILD_DATE)

.PHONY: test
test: ## Run tests
	go test -v -race -cover -coverpkg=./... -tags $(CGO_TAGS) ./...

.PHONY: lint
lint: ## Run linter
	golangci-lint --version
	golangci-lint run -v

.PHONY: build
build: ## Build the pure Go binary (no cgo, no odbc driver)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY_NAME) $(BINARY_PATH)

.PHONY: build-cgo
build-cgo: ## Build the cgo binary with the odbc and duckdb drivers
	CGO_ENABLED=1 go build -trimpath -tags $(CGO_TAGS) -ldflags="$(LDFLAGS)" -o bin/$(BINARY_NAME)-cgo $(BINARY_PATH)

.PHONY: build-releaser
build-releaser: ## Build the binary
	goreleaser build --snapshot --clean --single-target

.PHONY: build-local
build-local: build ## Build the binary for local development
	docker build --build-arg GO_IMAGE -t $(BINARY_NAME):local -f local.Dockerfile .

.PHONY: help
help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
