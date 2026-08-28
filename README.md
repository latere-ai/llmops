# llmops

[![ci](https://github.com/latere-ai/llmops/actions/workflows/ci.yml/badge.svg)](https://github.com/latere-ai/llmops/actions/workflows/ci.yml)
[![go](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

Latere's end-to-end inference layer for open-weights models. Instead of
renting model access through a router (OpenRouter et al.), this repo owns
the full path: weights frozen and checksummed, inference engines running
on GPUs we control, OpenAI- and Anthropic-compatible endpoints registered
behind Lux, the Latere model gateway.

Two deploy modes share one manifest schema and one serving contract:
Kubernetes for the multi-GPU fleet, and an installed binary under systemd
for a single-GPU host with no cluster around it.

## Why own the inference layer

- **Weight freeze.** Upstream Hugging Face repos mutate and disappear.
  Every model we serve is mirrored once into versioned, checksummed S3
  prefixes and never re-downloaded from upstream.
- **Cost and control.** Serving on our own GPUs beats per-token router
  pricing at sustained load, and removes third-party rate limits, silent
  model swaps, and data egress to routers.
- **A path to post-training.** Frozen weights we control are the
  prerequisite for fine-tuning/RL later (post-training itself is out of
  scope here; see `specs/`).

## Target models

| Model | Vendor | Class | Notes |
|---|---|---|---|
| MiniMax M3 | MiniMax | MoE | |
| GLM-5.2 | Zhipu AI | MoE | |
| Kimi K2.7 Code | Moonshot AI | MoE | |
| DeepSeek V4 Pro | DeepSeek | MoE | blocked on hardware |
| DeepSeek V4 Flash 0731 | DeepSeek | MoE | speculative decoding (DSpark) |
| Kimi K3 | Moonshot AI | MoE | multimodal; blocked on hardware + license |

Exact revisions, weight formats, and GPU footprints are pinned per model
in `specs/` and in each model's manifest under `models/`. "Blocked" means
the weights are frozen and the manifest is checked in, but no endpoint
exists yet — see the model's spec for what the block is.

## Repo layout

```
specs/      design specs (start at specs/README.md)
models/     per-model manifests: pinned HF revision, S3 prefix, engine config
deploy/     k8s LeaderWorkerSet manifests, one per model
cmd/        mirror (HF → S3 freeze), runtime (container entrypoint), bench
internal/   manifest schema, mirror logic, runtime shim, bench, deploycheck
```

Operators: [`DEPLOY.md`](./DEPLOY.md) is the end-to-end guide — build
images, freeze weights, deploy, and every configuration knob.

## Build

Go 1.27 or newer, no cgo, no other build dependency:

```sh
git clone https://github.com/latere-ai/llmops.git
cd llmops
make build                       # go build ./...
go build -o bin/ ./cmd/...       # mirror, runtime, bench binaries
```

`make hooks` installs the pre-commit gofmt and modernizer guard.

## Usage

Freeze a model into S3 (one-time per revision). `--bucket` takes any
s5cmd-reachable root, so AWS S3, DO Spaces, R2 and MinIO all work:

```sh
go run ./cmd/mirror pull moonshotai/Kimi-K2.7-Code --dir /scratch/kimi
go run ./cmd/mirror push moonshotai/Kimi-K2.7-Code@<sha> \
    --dir /scratch/kimi --bucket s3://<your-bucket>
go run ./cmd/mirror verify s3://<your-bucket>/moonshotai/Kimi-K2.7-Code/<sha>/
```

Validate manifests, then serve. `runtime serve` is the container
entrypoint, so on a GPU node it runs from the built image rather than
from a checkout:

```sh
go run ./cmd/runtime validate models/
bin/runtime serve --manifest /etc/llmops/model.yaml
```

Each model endpoint speaks OpenAI Chat natively (engine passthrough)
and Anthropic Messages at `/anthropic/v1/messages` via the shared
[`latere.ai/x/pkg/llmdialect`](https://github.com/latere-ai/pkg)
translator; the Lux dialect is served by Lux itself, which embeds the
same package.

Benchmark a live endpoint:

```sh
go run ./cmd/bench --url http://kimi-k2-7-code.llmops.svc:8000 \
    --model kimi-k2.7-code --concurrency 8 --requests 32 --out report.json
```

## Testing

```sh
make test        # go vet + go test ./...
make cover       # same, with a ≥90% total-statement coverage gate
make validate    # every models/*.yaml plus its deploy/*/lws.yaml
make e2e         # mirror pull→push→verify and runtime serve→ready→metrics
make e2e-local   # the whole pipeline on a laptop
```

`make test`, `make cover`, `make validate` and the lint targets need
nothing but a Go toolchain, and CI runs all of them on every push. `make
e2e` is a subset of the same suite: it drives the real code paths against
in-process fakes, so it needs no GPU, no network and no credentials.
Nothing in these targets is skipped for missing configuration.

`make e2e-local` is the only target with external prerequisites: Docker
or Podman, [uv](https://docs.astral.sh/uv/), and a one-time download of
roughly 1.5 GB. It runs the full pipeline against MinIO for S3 and a 0.6B
model under mlx, at zero cloud cost. It also needs Apple silicon, since
mlx is the local engine.

GPU serving on real hardware is not covered by any automated suite. It is
a per-model release gate, run by hand against the model's spec.

## Status

Working today: the mirror tool, the runtime entrypoint and health shim,
the bench harness, pinned manifests for all six models, and their
LeaderWorkerSet deploys. The manifest/deploy consistency check runs in
CI, and the whole pipeline is exercised end to end on a laptop.

Not yet done: the actual multi-hundred-GB mirrors, the GPU deployments,
and gateway registration. The per-model specs record what each one is
blocked on. The monitoring-plane specs (numbers 012 through 016) are
design only, with no code in this repo yet.

APIs are not frozen. The manifest schema, the shim's endpoints and the
CLI flags may change while the first models are brought up.

## License

MIT. See [LICENSE](./LICENSE).

Read [`specs/README.md`](./specs/README.md) for the roadmap and the
design records behind each decision.
