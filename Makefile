SHELL := /bin/bash
.DEFAULT_GOAL := help
.PHONY: help setup install build dev clean uninstall format tidy format-check lint test test-race test-integration release bump-major bump-minor bump-patch release-commit tag

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
	@"$(CURDIR)/.dev/bin/data-mate" --root "$(CURDIR)/.dev/data-mate" $(ARGS)

uninstall: ## Remove this installation; PURGE=1 also removes profiles and credentials
	@./scripts/uninstall.sh

clean: ## Alias for uninstall without purge
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

test-integration: ## Run owned PostgreSQL 16/18 Docker fixtures (never in CI)
	@./scripts/test-integration.sh

release: ## Build signed/notarized production artifacts (explicit signing settings required)
	@./scripts/release.sh

bump-major: ## Bump VERSION major and roll CHANGELOG; no Git mutations
	@./scripts/release-tools.sh bump major

bump-minor: ## Bump VERSION minor and roll CHANGELOG; no Git mutations
	@./scripts/release-tools.sh bump minor

bump-patch: ## Bump VERSION patch and roll CHANGELOG; no Git mutations
	@./scripts/release-tools.sh bump patch

release-commit: ## Preview, stage, commit and push release files (YES=1 confirms)
	@./scripts/release-tools.sh commit $(if $(filter 1 true yes,$(YES)),--yes)

tag: ## On clean latest main, confirm CAPTCHA and push the release tag for publication
	@./scripts/release-tools.sh tag
