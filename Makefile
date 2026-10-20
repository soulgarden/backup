.DEFAULT_GOAL := help
DOCKER ?= docker
HELM ?= helm
PYTHON ?= python3
IMAGE ?= soulgarden/backup
VERSION := $(shell cat VERSION)
PLATFORM ?= linux/amd64
POSTGRES_MAJOR ?= 16
IMAGE_TAG := $(IMAGE):$(VERSION)
.PHONY: help build push test integration lint
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
build: ## Build without publishing
	$(DOCKER) build --pull --platform "$(PLATFORM)" --build-arg POSTGRES_MAJOR="$(POSTGRES_MAJOR)" --tag "$(IMAGE_TAG)" .
push: ## Explicitly publish the tested local image
	$(DOCKER) push "$(IMAGE_TAG)"
test: ## Unit, storage and chart conformance tests
	$(PYTHON) -m pytest --cov=backup --cov-branch --cov-report=term-missing --cov-report=xml
integration: ## Synthetic PG16/S3/Elasticsearch and PID1 tests
	$(PYTHON) scripts/integration.py --image "$(IMAGE_TAG)"
lint: ## Validate charts and version wiring
	$(PYTHON) -m compileall -q backup
	$(HELM) lint --strict helm/backup
	$(PYTHON) scripts/check_version.py
