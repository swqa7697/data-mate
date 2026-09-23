SHELL := /bin/bash
.DEFAULT_GOAL := help
.PHONY: help setup install build dev clean uninstall format tidy format-check lint test test-race test-integration

VERBOSE ?= 0
export VERBOSE

help: ## Show available development commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Prepare pinned Go dependencies/tools without building the application
	@./scripts/setup.sh

install: ## Download pinned Go dependencies/tools and install .dev/bin/data-mate
	@./scripts/install.sh

build: ## Atomically rebuild .dev/bin/data-mate
	@./scripts/build.sh

dev: ## Run the installed binary (ARGS="help")
	@"$(CURDIR)/.dev/bin/data-mate" --root "$(CURDIR)/.dev" $(ARGS)

uninstall: ## Not ready until P11; PURGE=1 will opt into credential cleanup
	@./scripts/uninstall.sh

clean: ## Alias for uninstall without purge (not ready until P11)
	@$(MAKE) uninstall PURGE=0

format: ## Format Go and shell source using pinned tooling
	@./scripts/format.sh

tidy: format ## Alias for format

format-check: ## Check formatting without writing files
	@./scripts/format.sh --check

lint: ## Run go vet and pinned staticcheck
	@./scripts/lint.sh

test: ## Run isolated offline unit and regression tests
	@./scripts/test.sh

test-race: ## Run the same suite with Go's race detector
	@./scripts/test.sh -race

test-integration: ## Explicit Docker suite (not ready until P3; never runs in CI)
	@./scripts/test-integration.sh
