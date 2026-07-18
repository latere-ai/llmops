GO ?= go
COVER_MIN = 90.0

.PHONY: build test cover validate fmt vet e2e images clean

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

fmt:
	gofmt -l . | tee /dev/stderr | wc -l | grep -q '^ *0$$'

vet:
	$(GO) vet ./...

# CI-runnable e2e: full mirror pull→push→verify and runtime
# serve→ready→metrics paths against fakes (no GPU, no network).
# GPU e2e (make e2e-<model>) is a release gate on real hardware.
e2e:
	$(GO) test ./... -run 'E2E' -v

images:
	docker build -f Dockerfile.sglang -t ghcr.io/latere-ai/open-llms-runtime-sglang:dev .
	docker build -f Dockerfile.vllm -t ghcr.io/latere-ai/open-llms-runtime-vllm:dev .
	docker build -f Dockerfile.mirror -t ghcr.io/latere-ai/open-llms-mirror:dev .

clean:
	rm -f coverage.out

# Full local e2e: real small model + MinIO + mlx_lm engine (specs/011).
# Needs Docker and uv; zero cloud credentials. ~1.5 GB one-time download.
e2e-local:
	bash e2e/local/run.sh
