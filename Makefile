GO ?= go
COVER_MIN = 90.0
# Image registry/namespace prefix — override for Nexus, ECR, Harbor, etc.
# e.g. make release VERSION=v0.1.0 REGISTRY=123456789.dkr.ecr.eu-central-1.amazonaws.com/latere
REGISTRY ?= ghcr.io/latere-ai

.PHONY: build test cover validate fmt fmt-check hooks vet e2e images dist clean lint-modernize

build:
	$(GO) build ./...

test: vet
	$(GO) test ./...

# Coverage gate: total statement coverage must be >= $(COVER_MIN)%.
cover:
	$(GO) test ./... -coverprofile=coverage.out
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$$3); print $$3}'); \
	echo "total coverage: $$total% (min $(COVER_MIN)%)"; \
	awk "BEGIN {exit !($$total >= $(COVER_MIN))}" || { echo "FAIL: coverage below $(COVER_MIN)%"; exit 1; }

# Validate all model manifests + deploy consistency (also run in `test`
# via internal/deploycheck).
validate:
	$(GO) run ./cmd/llmops validate models/

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

# Versioned build + push of all four images (see DEPLOY.md).
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
