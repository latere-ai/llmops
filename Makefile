GO ?= go
COVER_MIN = 90.0
# Image registry/namespace prefix — override for Nexus, ECR, Harbor, etc.
# e.g. make release VERSION=v0.1.0 REGISTRY=123456789.dkr.ecr.eu-central-1.amazonaws.com/latere
REGISTRY ?= ghcr.io/latere-ai

.PHONY: build test cover validate fmt fmt-check hooks vet e2e images clean

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
	$(GO) run ./cmd/runtime validate models/

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

# CI-runnable e2e: full mirror pull→push→verify and runtime
# serve→ready→metrics paths against fakes (no GPU, no network).
# GPU e2e (make e2e-<model>) is a release gate on real hardware.
e2e:
	$(GO) test ./... -run 'E2E' -v

images:
	docker build -f Dockerfile.sglang -t $(REGISTRY)/open-llms-runtime-sglang:dev .
	docker build -f Dockerfile.sglang-k3 -t $(REGISTRY)/open-llms-runtime-sglang-k3:dev .
	docker build -f Dockerfile.vllm -t $(REGISTRY)/open-llms-runtime-vllm:dev .
	docker build -f Dockerfile.mirror -t $(REGISTRY)/open-llms-mirror:dev .

# Versioned build + push of all four images (see DEPLOY.md).
# Usage: make release VERSION=v0.1.0 [REGISTRY=...]
# (requires docker login against $(REGISTRY))
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z [REGISTRY=...]"; exit 1; }
	docker build --platform linux/amd64 -f Dockerfile.sglang -t $(REGISTRY)/open-llms-runtime-sglang:$(VERSION) .
	docker build --platform linux/amd64 -f Dockerfile.sglang-k3 -t $(REGISTRY)/open-llms-runtime-sglang-k3:$(VERSION) .
	docker build --platform linux/amd64 -f Dockerfile.vllm -t $(REGISTRY)/open-llms-runtime-vllm:$(VERSION) .
	docker build --platform linux/amd64 -f Dockerfile.mirror -t $(REGISTRY)/open-llms-mirror:$(VERSION) .
	docker push $(REGISTRY)/open-llms-runtime-sglang:$(VERSION)
	docker push $(REGISTRY)/open-llms-runtime-sglang-k3:$(VERSION)
	docker push $(REGISTRY)/open-llms-runtime-vllm:$(VERSION)
	docker push $(REGISTRY)/open-llms-mirror:$(VERSION)

clean:
	rm -f coverage.out

# Full local e2e: real small model + MinIO + mlx_lm engine (specs/011).
# Needs Docker and uv; zero cloud credentials. ~1.5 GB one-time download.
e2e-local:
	bash e2e/local/run.sh
