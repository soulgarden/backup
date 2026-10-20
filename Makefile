.DEFAULT_GOAL := help

DOCKER ?= docker
HELM ?= helm
IMAGE ?= soulgarden/backup
VERSION ?= $(shell sed -n 's/^appVersion: "\(.*\)"/\1/p' helm/backup/Chart.yaml)
PLATFORM ?= linux/amd64
POSTGRES_MAJOR ?= 16
ENV_FILE ?= .env
S3_CONFIG ?= $(CURDIR)/.s3cfg.local
IMAGE_TAG := $(IMAGE):$(VERSION)

.PHONY: help build push run lint

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the image locally
	$(DOCKER) build --pull --platform "$(PLATFORM)" \
		--build-arg POSTGRES_MAJOR="$(POSTGRES_MAJOR)" --tag "$(IMAGE_TAG)" .

push: ## Push an already built image
	$(DOCKER) push "$(IMAGE_TAG)"

run: ## Run a backup using .env and .s3cfg.local
	@test -f "$(ENV_FILE)" || { echo "Missing $(ENV_FILE)" >&2; exit 1; }
	@test -f "$(S3_CONFIG)" || { echo "Missing $(S3_CONFIG)" >&2; exit 1; }
	$(DOCKER) run --rm --platform "$(PLATFORM)" --user "$$(id -u):$$(id -g)" \
		--env-file "$(ENV_FILE)" \
		--mount "type=bind,source=$(S3_CONFIG),target=/home/backup/.s3cfg,readonly" \
		"$(IMAGE_TAG)"

lint: ## Check shell syntax and the Helm chart
	sh -n docker-entrypoint.sh
	$(HELM) lint --strict helm/backup
