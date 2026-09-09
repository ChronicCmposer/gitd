# gitd Makefile facade — thin wrappers over the Bazel build + dist pipeline.
#
# Phase 2 makes publish-openssh-dist and check-openssh-dist-deps real and adds
# the analogous per-product determinism/publish targets (2.1-2.4). Targets
# that land in later phases (mutate, mutate-ci, deploy) remain loud stubs.

SHELL := /bin/bash
DIST := tools/dist

.PHONY: build test fuzz mutate mutate-ci coverage \
        check-openssh-dist check-git-dist check-fish-dist check-sudo-dist \
        check-ca-certs-dist check-containerd-dist check-runc-dist \
        check-pinned-go-dist check-openssh-dist-deps gen-dist-pins \
        publish-openssh-dist publish-git-dist publish-fish-dist \
        publish-sudo-dist publish-ca-certs-dist publish-containerd-dist \
        publish-runc-dist image image-container deploy

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

# --- Dist pipeline (Phase 2) -------------------------------------------------

# check-*-dist: build-twice determinism gate (R3-Q2, 2.3). Fails loudly when
# the two builds are not byte-identical.
check-openssh-dist: ## Determinism check for the openssh artifact.
	$(DIST)/check-dist.sh openssh

check-git-dist: ## Determinism check for the git artifact.
	$(DIST)/check-dist.sh git

check-fish-dist: ## Determinism check for the fish artifact.
	$(DIST)/check-dist.sh fish

check-sudo-dist: ## Determinism check for the sudo artifact.
	$(DIST)/check-dist.sh sudo

check-ca-certs-dist: ## Determinism check for the ca-certificates artifact.
	$(DIST)/check-dist.sh ca-certs

check-containerd-dist: ## Determinism check for the containerd artifact.
	$(DIST)/check-dist.sh containerd

check-runc-dist: ## Determinism check for the runc artifact.
	$(DIST)/check-dist.sh runc

check-pinned-go-dist: ## Verify the pinned Go toolchain (2.3).
	$(DIST)/build-pinned-go.sh

# check-openssh-dist-deps: dependency-hygiene check for the openssh pipeline.
# Root + curl + tar are the real prerequisites; patch applicability is verified
# by check-openssh-dist itself (the build applies the patch to the pinned
# source and fails loudly on a mismatch).
check-openssh-dist-deps: ## Check openssh dist pipeline prerequisites.
	@test "$$(id -u)" -eq 0 || { echo "gitd: openssh dist builds need root (chroot)"; exit 1; }
	@command -v curl >/dev/null || { echo "gitd: curl is required for the openssh dist build"; exit 1; }
	@command -v tar >/dev/null || { echo "gitd: tar is required for the openssh dist build"; exit 1; }
	@echo "gitd: openssh dist prerequisites OK (root + curl + tar)"

# gen-dist-pins: regenerate //:dist_pins.bzl sha256 pins from built tarballs.
gen-dist-pins: ## Regenerate dist_pins.bzl from the built tarballs.
	$(DIST)/gen-dist-pins.sh

# publish-*-dist: upload a determinism-checked artifact to GitHub Releases
# (primary) + S3 (fallback). Requires GH_TOKEN + aws credentials.
publish-openssh-dist: ## Publish the openssh dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh openssh

publish-git-dist: ## Publish the git dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh git

publish-fish-dist: ## Publish the fish dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh fish

publish-sudo-dist: ## Publish the sudo dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh sudo

publish-ca-certs-dist: ## Publish the ca-certificates dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh ca-certs

publish-containerd-dist: ## Publish the containerd dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh containerd

publish-runc-dist: ## Publish the runc dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh runc

# --- Image (2.5) --------------------------------------------------------------

image: ## Build the from-scratch OCI image (requires published dist tarballs).
	bazel build //image:image

image-container: ## Produce gitd-container.tar for `ctr images import` (R3-Q2).
	bazel build //image:image
	$(DIST)/package-image.sh

deploy: ## Deploy the stack via CloudFormation (Phase 7). Usage: make deploy ARGS="--key-name KP --eip-allocation-id EIP"
	./cloudformation/deploy.sh $(ARGS)
