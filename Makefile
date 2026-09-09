# gitd Makefile facade — thin wrappers over the Bazel build.
#
# Targets that land in later phases (mutate, mutate-ci, publish-openssh-dist,
# check-openssh-dist-deps, deploy) exist now as loud stubs so the facade names
# are stable; each prints "not yet implemented in this phase" and exits 1.

SHELL := /bin/bash

.PHONY: build test fuzz mutate mutate-ci coverage publish-openssh-dist check-openssh-dist-deps deploy

build: ## Build everything.
	bazel build //...

test: ## Run all tests.
	bazel test //...

fuzz: ## Fuzz the SSH_ORIGINAL_COMMAND tokenizer (rules_go has no native fuzz support).
	bazel run @io_bazel_rules_go//go -- test -fuzz=FuzzParseCommand ./internal/sshcmd/...

mutate: ## Run mutation testing on changed lines (Phase 9).
	@echo "gitd: mutation testing not yet implemented in this phase (Phase 9)"

mutate-ci: ## CI mutation gate: zero new survivors on changed lines (Phase 9).
	@echo "gitd: mutation CI gate not yet implemented in this phase (Phase 9)"

coverage: ## Statement coverage via the pinned rules_go SDK.
	bazel run @io_bazel_rules_go//go -- test -coverprofile=/tmp/gitd-cover.out ./internal/...
	bazel run @io_bazel_rules_go//go -- tool cover -func=/tmp/gitd-cover.out

publish-openssh-dist: ## Publish the openssh dist artifact to S3 + GitHub (Phase 2).
	@echo "gitd: openssh dist publishing not yet implemented in this phase (Phase 2)"

check-openssh-dist-deps: ## Dependency-hygiene check for the openssh dist pipeline (Phase 2).
	@echo "gitd: openssh dist dependency check not yet implemented in this phase (Phase 2)"

deploy: ## Deploy the stack via CloudFormation (Phase 4+).
	@echo "gitd: deploy not yet implemented in this phase"