GO ?= go
# Image registry/namespace prefix — override for Nexus, ECR, Harbor, etc.
# e.g. make release VERSION=v0.1.0 REGISTRY=123456789.dkr.ecr.eu-central-1.amazonaws.com/latere
REGISTRY ?= ghcr.io/latere-ai

.PHONY: build test cover test-hermetic test-race validate deps cgo-free spec-lint fmt fmt-check hooks vet e2e images dist clean lint-modernize lint-config lint lint-otel

build:
	$(GO) build ./...

test: lint-config vet
	$(GO) test ./...

# Coverage gate, per package rather than as a repository average: an
# average lets a well-tested package carry an untested one and reports a
# number nobody can act on. The threshold and any exemptions, each with
# its reason, live in .lateregate.yaml.
cover:
	$(GO) test ./... -coverprofile=coverage.out -coverpkg=./...
	@$(GO) tool lateregate cover -profile=coverage.out

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
# The directories kept on PATH are named in .lateregate.yaml, so a test
# that needs a system tool has to say so.
test-hermetic:
	@$(GO) tool lateregate hermetic

# The race detector needs cgo, which the shipped binaries do not: this is
# about finding a race in the test harness, not about what we compile to.
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

# Validate all model manifests + deploy consistency (also run in `test`
# via internal/deploycheck). The spec tree is a separate target now; see
# spec-lint below.
validate: deps cgo-free
	$(GO) run ./cmd/llmops validate models/

# The CLI's dependency footprint. A dependency arriving is not a failure; one
# nobody decided on is, and the first symptom is otherwise a slower build.
# The allowlist, whose value is the reason, lives in .lateregate.yaml.
deps:
	@$(GO) tool lateregate depcheck

# `make dist` builds with CGO_ENABLED=0 and docs/development.md says "no cgo".
# A claim in a user-facing document with no gate behind it goes stale silently.
# Reads source rather than the build, because a file can import "C" behind a
# build tag this platform does not select and still be a violation.
cgo-free:
	@$(GO) tool lateregate cgo-free

# The spec tree checks: frontmatter, the closed status vocabulary, the index
# rows, dependency edges and wikilinks. Conventions live in .lateregate.yaml.
spec-lint:
	@$(GO) tool lateregate spec-lint

# fmt formats all Go sources in place.
fmt:
	gofmt -w .

# fmt-check fails if any Go source is not gofmt-formatted.
fmt-check:
	@$(GO) tool lateregate fmt-check

# hooks installs the repository git hooks (pre-commit gofmt guard).
hooks:
	git config core.hooksPath .githooks
	@echo "installed git hooks (core.hooksPath=.githooks)"

vet:
	$(GO) vet ./...

# lint-otel keeps outbound HTTP instrumented so traces propagate across
# services. It fails on two shapes: an &http.Client{ ... } composite literal
# whose body sets no Transport field, and any use of http.DefaultClient. The awk
# program walks from the opening brace to its matching close brace, so a
# Transport field several lines down still counts. Strings and comments are
# removed before matching, so writing about this rule does not trip it. Brace
# character classes are bracketed so mawk, the awk on ubuntu-latest, parses them.
lint-otel:
	@bad=$$(find cmd internal -name '*.go' ! -name '*_test.go' -exec awk 'function strip(l, i, n, c, d, out) { n = length(l); out = ""; i = 1; while (i <= n) { c = substr(l, i, 1); d = substr(l, i + 1, 1); if (blk) { if (c == "*" && d == "/") { blk = 0; i += 2 } else { i++ } } else if (q != "") { out = out c; if (c == q) q = ""; i++ } else if (c == "\"" || c == "`") { q = c; out = out c; i++ } else if (c == "/" && d == "/") { break } else if (c == "/" && d == "*") { blk = 1; i += 2 } else { out = out c; i++ } } return out } FNR == 1 { blk = 0; q = "" } { c = strip($$0); if (index(c, "http.DefaultClient")) print FILENAME ":" FNR ": http.DefaultClient is not instrumented; use otel.HTTPClient()"; p = index(c, "&http.Client{"); if (p == 0) next; start = FNR; s = substr(c, p + 13); body = s; depth = 1; while (1) { depth += gsub(/[{]/, "{", s) - gsub(/[}]/, "}", s); if (depth <= 0) break; if ((getline) <= 0) break; s = strip($$0); if (index(s, "http.DefaultClient")) print FILENAME ":" FNR ": http.DefaultClient is not instrumented; use otel.HTTPClient()"; body = body s } if (body !~ /Transport/) print FILENAME ":" start ": bare &http.Client{...} literal sets no Transport; use otel.Transport(...)" }' {} +); \
	if [ -n "$$bad" ]; then \
		echo "uninstrumented outbound HTTP client:" >&2; \
		echo "$$bad" >&2; \
		exit 1; \
	fi

# lint-modernize fails on code that a standard library call already covers.
# It runs the toolchain modernizers, which overlap golangci-lint's modernize
# linter but add three it does not carry: buildtag, hostport, and the
# go:fix inline directives. newexpr and errorsastype are off for the reasons
# recorded in .golangci.yml and named in .lateregate.yaml; lateregate checks
# each still exists, because `go fix` rejects an unknown -name=false and the
# gate would then pass silently.
lint-modernize:
	@$(GO) tool lateregate modernize

# .golangci.yml is generated and gitignored: golangci-lint cannot inherit a
# shared config, so it is rendered from latere.ai/x/ci-gate on every run.
# Regenerating rather than committing is what makes divergence impossible
# instead of merely detectable.
lint-config:
	@$(GO) tool lateregate golangci

# Runs the linter the CI lint job runs, against the config lint-config renders.
# Without this the only machine that ever lints this repo is a runner, which is
# the shape these gates exist to avoid.
GOLANGCI_VERSION ?= v2.13.1

lint: lint-config lint-otel
	@$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

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
