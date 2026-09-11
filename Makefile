# gitd Makefile facade — thin wrappers over the Bazel build + dist pipeline.
#
# Phase 2 makes publish-openssh-dist and check-openssh-dist-deps real and adds
# the analogous per-product determinism/publish targets (2.1-2.4). Phase 9
# makes mutate, mutate-ci, and coverage real (R4-Q1..R4-Q4).

SHELL := /bin/bash
DIST := tools/dist
MUTATION := tools/mutation

.PHONY: build test fuzz lint fmt-check mutate mutate-ci coverage \
        check-openssh-dist check-git-dist check-fish-dist \
        check-ca-certs-dist check-containerd-dist check-runc-dist \
        check-pinned-go-dist check-openssh-dist-deps gen-dist-pins \
        publish-openssh-dist publish-git-dist publish-fish-dist \
        publish-ca-certs-dist publish-containerd-dist \
        publish-runc-dist image image-container deploy update create-bucket \
        check-deps version bump-version release

build: ## Build everything.
	bazel build //...

test: ## Run all tests.
	bazel test //...

# FUZZTIME bounds each fuzz target's run (a 30s CI-friendly budget by
# default; override for long local runs, e.g. `make fuzz FUZZTIME=5m`).
# rules_go has no native fuzz support, so each fuzzer is a `go test -fuzz=Fn`
# run through the pinned rules_go SDK go binary.
FUZZTIME ?= 30s
FUZZ_RUN := bazel run @io_bazel_rules_go//go -- test

fuzz: ## Fuzz the SSH tokenizer (R3-Q9) and the event decoder (R5-Q9), each bounded by FUZZTIME.
	$(FUZZ_RUN) -fuzz=FuzzParseCommand -fuzztime=$(FUZZTIME) ./internal/sshcmd/...
	$(FUZZ_RUN) -fuzz=FuzzDecodeEvent -fuzztime=$(FUZZTIME) ./internal/event/...

# golangci-lint is pinned at a standalone-binary version and run via a thin
# tools/lint.sh wrapper (dev-only, never added to the Go module). See
# tools/lint.sh for the exact pin.
lint: ## Run golangci-lint (pinned v2) over the module (Design Conventions: lint gate).
	tools/lint.sh

# gofmt enforcement (Design Conventions): fail loudly when any file is not
# gofmt-formatted; `gofmt -l .` prints exactly the offenders and is empty on
# success.
fmt-check: ## Fail if any Go file is not gofmt-formatted.
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gitd: gofmt needed on:"; echo "$$out"; exit 1; fi

mutate: ## Full baseline-aware mutation run over all internal/ packages (Phase 9, R4-Q1/Q3).
	$(MUTATION)/mutate.sh

mutate-ci: ## CI mutation gate: git-diff mode, zero NEW survivors (Phase 9, R4-Q3). BASE=origin/main
	$(MUTATION)/mutate-ci.sh $(BASE)

coverage: ## Statement-coverage floor >=80% per internal package (Phase 9, R4-Q4).
	tools/coverage/coverage.sh

check-deps: ## Dependency hygiene: go mod verify + govulncheck (R3-Q8).
	tools/check-deps

# --- Dist pipeline (Phase 2) -------------------------------------------------

# check-*-dist: build-twice determinism gate (R3-Q2, 2.3). Fails loudly when
# the two builds are not byte-identical.
check-openssh-dist: ## Determinism check for the openssh artifact.
	$(DIST)/check-dist.sh openssh

check-git-dist: ## Determinism check for the git artifact.
	$(DIST)/check-dist.sh git

check-fish-dist: ## Determinism check for the fish artifact.
	$(DIST)/check-dist.sh fish

check-ca-certs-dist: ## Determinism check for the ca-certificates artifact.
	$(DIST)/check-dist.sh ca-certs

check-containerd-dist: ## Determinism check for the containerd artifact.
	$(DIST)/check-dist.sh containerd

check-runc-dist: ## Determinism check for the runc artifact.
	$(DIST)/check-dist.sh runc

check-pinned-go-dist: ## Verify the pinned Go toolchain (2.3).
	$(DIST)/build-pinned-go.sh

# check-openssh-dist-deps: dependency-hygiene check for the openssh pipeline.
# Real root, passwordless sudo, or working unprivileged user namespaces (for
# non-root chroot builds) + curl + tar are the prerequisites; patch
# applicability is verified by check-openssh-dist itself (the build applies
# the patch to the pinned source and fails loudly on a mismatch).
check-openssh-dist-deps: ## Check openssh dist pipeline prerequisites.
	@if [ "$$(id -u)" -eq 0 ]; then \
		echo "gitd: openssh dist prerequisites OK (root + curl + tar)"; \
	elif command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then \
		echo "gitd: openssh dist prerequisites OK (passwordless sudo + curl + tar)"; \
	elif command -v unshare >/dev/null 2>&1 && unshare -Urmpf true >/dev/null 2>&1; then \
		echo "gitd: openssh dist prerequisites OK (rootless userns + curl + tar)"; \
	else \
		echo "gitd: openssh dist builds need root, passwordless sudo, or working unprivileged user namespaces (set kernel.unprivileged_userns_clone=1 / user.max_user_namespaces, or run as root)"; \
		exit 1; \
	fi
	@command -v curl >/dev/null || { echo "gitd: curl is required for the openssh dist build"; exit 1; }
	@command -v tar >/dev/null || { echo "gitd: tar is required for the openssh dist build"; exit 1; }

# gen-dist-pins: regenerate //:dist_pins.bzl sha256 pins from built tarballs.
gen-dist-pins: ## Regenerate dist_pins.bzl from the built tarballs.
	$(DIST)/gen-dist-pins.sh

# publish-*-dist: upload a determinism-checked artifact to GitHub Releases
# (primary) + S3 (fallback). Requires an authenticated gh CLI (gh auth login) + aws credentials.
publish-openssh-dist: ## Publish the openssh dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh openssh

publish-git-dist: ## Publish the git dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh git

publish-fish-dist: ## Publish the fish dist artifact to GitHub + S3.
	$(DIST)/publish-dist.sh fish

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

deploy: ## Deploy the stack via CloudFormation (Phase 7). Usage: make deploy ARGS="--key-name KP"
	./cloudformation/deploy.sh $(ARGS)

update: ## In-place update of a live server (Phase 8.1, R3-Q10/R6-Q3). Image must be prebuilt + signed by `make release`; update VERIFIES the signature + pinned sha256 (it does not sign) and verifies the remote roll over SSM (success sentinel / EXIT_CODE: 0) before reporting success. Usage: make update ARGS="--sha256 <hex>"
	./cloudformation/update.sh $(ARGS)

create-bucket: ## Provision the git.cmposer.cc S3 bucket before the first deploy (deploy-before-stack ordering).
	./cloudformation/create-bucket.sh

# --- Versioning & releases (R1-Q11/Q14) ---------------------------------------

# Git tags are the source of truth for versioning; the version is injected at
# link time via bazel stamping (workspace-status.sh -> STABLE_VERSION -> x_defs),
# never generated. These three targets are the version/release facade.
version: ## Print the current version (exact release tag at HEAD, else v0.0.0-devel).
	@git describe --tags --exact-match 2>/dev/null || echo v0.0.0-devel

bump-version: ## Tag the next release (git tags are the source of truth). Usage: make bump-version LEVEL=<major|minor|patch>
	./tools/release/bump-version.sh $(LEVEL)

release: ## Build + publish a release from the exact-tagged HEAD. Requires an exact tag at HEAD; prints the update pin sha256.
	./tools/release/release.sh
