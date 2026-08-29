GO ?= go
# Image registry/namespace prefix — override for Nexus, ECR, Harbor, etc.
# e.g. make release VERSION=v0.1.0 REGISTRY=123456789.dkr.ecr.eu-central-1.amazonaws.com/latere
REGISTRY ?= ghcr.io/latere-ai

.PHONY: build test cover test-hermetic test-race validate fmt fmt-check hooks vet e2e images dist clean lint-modernize

build:
	$(GO) build ./...

test: vet
	$(GO) test ./...

# Coverage gate, per package rather than as a repository average: an
# average lets a well-tested package carry an untested one and reports a
# number nobody can act on. Exemptions live in internal/covercheck with
# a reason attached.
cover:
	$(GO) test ./... -coverprofile=coverage.out -coverpkg=./...
	@$(GO) run ./internal/covercheck -profile=coverage.out

# Run the suite without the developer's PATH.
#
# Three CI failures in one day came from tests that depended on what
# happened to be installed on the machine running them: `systemctl`
# present-but-unprivileged on a runner and absent on macOS, and `claude`
# on a developer's PATH and not on a runner's. Each passed locally and
# failed in CI, which is the worst order to find out.
#
# Keeping only the Go toolchain and the system directories reproduces a
# runner's environment closely enough to catch that class before a push.
test-hermetic:
	@go_dir=$$(dirname $$(command -v $(GO))); \
	echo "PATH=$$go_dir:/usr/bin:/bin"; \
	env PATH="$$go_dir:/usr/bin:/bin" $(GO) test ./...

# The race detector needs cgo, which the shipped binaries do not: this is
# about finding a race in the test harness, not about what we compile to.
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

# Validate all model manifests + deploy consistency (also run in `test`
# via internal/deploycheck), then the spec tree that describes them.
#
# The spec lint exists because the index drifted: every row read `draft`
# while five specs were built and one was serving. It had been
# hand-edited a dozen times that day.
validate:
	$(GO) run ./cmd/llmops validate models/
	$(GO) test ./internal/speclint/

# fmt formats all Go sources in place.
fmt:
	gofmt -w .

# fmt-check fails if any Go source is not gofmt-formatted.
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt: unformatted files:"; echo "$$out"; exit 1; fi

# hooks installs the repository git hooks (pre-commit gofmt guard).
hooks:
	git config core.hooksPath .githooks
	@echo "installed git hooks (core.hooksPath=.githooks)"

vet:
	$(GO) vet ./...

# lint-modernize fails on code that a standard library call already covers.
# It runs the toolchain modernizers, which overlap golangci-lint's modernize
# linter but add three it does not carry: buildtag, hostport, and the
# go:fix inline directives. newexpr and errorsastype are off for the reasons
# recorded in .golangci.yml.
# Only a non-empty patch fails the target. go fix also exits non-zero when a
# package does not type-check, which is a build error rather than a finding,
# so stderr is dropped and the decision rests on the patch alone.
lint-modernize:
	@for fixer in newexpr errorsastype; do \
		$(GO) tool fix help 2>&1 | grep -q "^    $$fixer " || { \
			echo "go fix no longer carries the $$fixer fixer, so -$$fixer=false is rejected and this check passes silently"; \
			exit 1; \
		}; \
	done
	@patch=$$($(GO) fix -diff -newexpr=false -errorsastype=false ./... 2>/dev/null); \
	if [ -n "$$patch" ]; then \
		echo "$$patch"; \
		echo "go fix: the diff above is already in the standard library; apply it with go fix"; \
		exit 1; \
	fi

# CI-runnable e2e: full pull→push→verify and serve
# serve→ready→metrics paths against fakes (no GPU, no network).
# GPU e2e (make e2e-<model>) is a release gate on real hardware.
e2e:
	$(GO) test ./... -run 'E2E' -v

images:
	docker build -f Dockerfile.sglang -t $(REGISTRY)/llmops-runtime-sglang:dev .
	docker build -f Dockerfile.sglang --build-arg SGLANG_IMAGE=lmsysorg/sglang:kimi-k3-c6ad1f26-20260729-amd64 -t $(REGISTRY)/llmops-runtime-sglang-k3:dev .
	docker build -f Dockerfile.vllm -t $(REGISTRY)/llmops-runtime-vllm:dev .
	docker build -f Dockerfile.mirror -t $(REGISTRY)/llmops-mirror:dev .

# Versioned build + push of all four images (see docs/deploy.md).
# Usage: make release VERSION=v0.1.0 [REGISTRY=...]
# (requires docker login against $(REGISTRY))
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z [REGISTRY=...]"; exit 1; }
	docker build --platform linux/amd64 -f Dockerfile.sglang -t $(REGISTRY)/llmops-runtime-sglang:$(VERSION) .
	docker build --platform linux/amd64 -f Dockerfile.sglang --build-arg SGLANG_IMAGE=lmsysorg/sglang:kimi-k3-c6ad1f26-20260729-amd64 -t $(REGISTRY)/llmops-runtime-sglang-k3:$(VERSION) .
	docker build --platform linux/amd64 -f Dockerfile.vllm -t $(REGISTRY)/llmops-runtime-vllm:$(VERSION) .
	docker build --platform linux/amd64 -f Dockerfile.mirror -t $(REGISTRY)/llmops-mirror:$(VERSION) .
	docker push $(REGISTRY)/llmops-runtime-sglang:$(VERSION)
	docker push $(REGISTRY)/llmops-runtime-sglang-k3:$(VERSION)
	docker push $(REGISTRY)/llmops-runtime-vllm:$(VERSION)
	docker push $(REGISTRY)/llmops-mirror:$(VERSION)

# Host binaries for the bare-metal deploy mode (specs/020, specs/024).
# GOARCH must be set explicitly: a plain `go build` targets the builder,
# so building on a laptop for an arm64 host would silently ship an amd64
# binary. CGO stays off so these cross-compile with no toolchain per
# target.
#
# One binary per platform, not three: a host copied to by hand has one
# file to place and one thing to ask for a version (specs/024).
DIST_PLATFORMS = linux/amd64 linux/arm64
DIST_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
DIST_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DIST_LDFLAGS = -s -w -X main.version=$(DIST_VERSION) -X main.commit=$(DIST_COMMIT)

dist:
	@rm -rf dist
	@for p in $(DIST_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "dist: $$os/$$arch $(DIST_VERSION)"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build \
			-ldflags "$(DIST_LDFLAGS)" \
			-o dist/$$os-$$arch/llmops ./cmd/llmops || exit 1; \
	done
	@find dist -type f -exec ls -lh {} \; | awk '{print $$9, $$5}'

clean:
	rm -f coverage.out
	rm -rf dist

# Full local e2e: real small model + MinIO + mlx_lm engine (specs/011).
# Needs Docker and uv; zero cloud credentials. ~1.5 GB one-time download.
e2e-local:
	bash e2e/local/run.sh
