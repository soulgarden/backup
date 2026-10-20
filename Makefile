.DEFAULT_GOAL := help
DOCKER ?= docker
HELM ?= helm
GO ?= go
IMAGE ?= soulgarden/backup
VERSION := $(shell cat VERSION)
PLATFORM ?= linux/amd64
POSTGRES_MAJOR ?= 16
IMAGE_TAG := $(IMAGE):$(VERSION)
.PHONY: help build image push test test-race integration lint vet
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
build: ## Build the Go executable
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/backup ./cmd/backup
image: ## Build image without publishing
	$(DOCKER) build --platform "$(PLATFORM)" --build-arg POSTGRES_MAJOR="$(POSTGRES_MAJOR)" --tag "$(IMAGE_TAG)" .
push: ## Explicitly publish the tested local image
	$(DOCKER) push "$(IMAGE_TAG)"
test: ## Go tests with coverage
	$(GO) test -coverprofile=coverage.out ./internal/backup
	$(GO) tool cover -func=coverage.out
	@$(GO) tool cover -func=coverage.out | awk '/total:/ {gsub(/%/, "", $$3); if ($$3 < 90) exit 1}'
	$(GO) test ./cmd/backup

test-race: ## Detect data races
	$(GO) test -race ./...
vet: ## Go static checks
	$(GO) vet ./...
integration: ## Synthetic PG16/S3 and PID1 checks
	IMAGE="$(IMAGE_TAG)" scripts/integration.sh
lint: ## Formatting and chart validation
	@test -z "$$(gofmt -l cmd internal)"
	$(HELM) lint --strict helm/backup
