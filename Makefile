BINARY_NAME := dbq
BINARY_PATH := ./cmd/${BINARY_NAME}/main.go

BUILD_DATE := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
BUILD_COMMIT := $(shell git rev-parse --short HEAD)
VERSION := $(or $(IMAGE_TAG),$(shell git describe --tags --first-parent --match "v*" 2> /dev/null || echo v0.0.0))

.DEFAULT_GOAL := help

.PHONY: test
test: ## Run tests
	go test -v -race -cover -coverpkg=./... ./...

.PHONY: lint
lint: ## Run linter
	golangci-lint --version
	golangci-lint run -v

.PHONY: build
build: ## Build the binary
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(BUILD_COMMIT) -X main.date=$(BUILD_DATE)" -o bin/$(BINARY_NAME) $(BINARY_PATH)

.PHONY: build-releaser
build-releaser: ## Build the binary
	goreleaser build --snapshot --clean --single-target

.PHONY: build-local
build-local: build ## Build the binary for local development
	docker build --build-arg GO_IMAGE -t $(BINARY_NAME):local -f local.Dockerfile .

.PHONY: help
help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
