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
  Every model we serve is pinned to a revision and checksummed per file,
  then never re-downloaded from upstream — in an S3 prefix for the fleet,
  or in place on a host's own disk.
- **Cost and control.** Serving on our own GPUs beats per-token router
  pricing at sustained load, and removes third-party rate limits, silent
  model swaps, and data egress to routers.
- **A path to post-training.** Frozen weights we control are the
  prerequisite for fine-tuning/RL later (post-training itself is out of
  scope here; see `specs/`).

## Target models

One list, whichever way a model is deployed. **Deploy** is how each one
is run *today*, not a restriction on it — the mode is a manifest field,
and deploy mode and hardware are independent axes. A GB10 box can join a
cluster; an H200 host can be run bare-metal for a one-off. Switching a
model is a one-line manifest edit plus the artifact that mode uses.

| Model | Vendor | Class | Hardware | Deploy | Notes |
|---|---|---|---|---|---|
| MiniMax M3 | MiniMax | MoE | 8x H200 | k8s | |
| GLM-5.2 | Zhipu AI | MoE | 8x H200 | k8s | |
| Kimi K2.7 Code | Moonshot AI | MoE | 8x H200 | k8s | |
| DeepSeek V4 Pro | DeepSeek | MoE | 8x B200 | k8s | blocked on hardware |
| DeepSeek V4 Flash 0731 | DeepSeek | MoE | 8x B200 | k8s | speculative decoding (DSpark) |
| Kimi K3 | Moonshot AI | MoE | 8x B300 | k8s | multimodal; blocked on hardware + license |
| Qwen3.8-27B | Alibaba | dense | 1x GB10 | bare-metal | multimodal; BF16, no quantization |

Exact revisions, weight formats, and GPU footprints are pinned per model
in `specs/` and in each model's manifest under `models/`. "Blocked" means
the weights are frozen and the manifest is checked in, but no endpoint
exists yet — see the model's spec for what the block is.

Qwen3.8-27B is the one that is not frontier-scale MoE, on purpose. The
registry serves what we choose to own, and a model small enough to run
undamaged on a single GPU is a different product from a large one
squeezed to fit — not a lesser version of one.

## Repo layout

```
specs/      design specs (start at specs/README.md)
models/     per-model manifests: pinned HF revision, weight source, engine config
deploy/     one artifact per model: a k8s LeaderWorkerSet, or a systemd unit
cmd/        llmops — the one command (weights, serving, bench)
internal/   manifest schema, mirror logic, runtime shim, bench, deploycheck
```

Operators: [`DEPLOY.md`](./DEPLOY.md) is the end-to-end guide — build
images, freeze weights, deploy, and every configuration knob.
[`PRACTICE.md`](./PRACTICE.md) is how to decide whether a model belongs
on a given GPU at all: what sets fit, what sets speed, and the measured
numbers for the hardware we run.

## Build

Go 1.27 or newer, no cgo, no other build dependency:

```sh
git clone https://github.com/latere-ai/llmops.git
cd llmops
make build                       # go build ./...
make dist                        # static linux/amd64 + linux/arm64 binaries
```

`make hooks` installs the pre-commit gofmt and modernizer guard.

## Usage

Everything is one command:

```
llmops pull     <hf_repo>[@revision] --dir <dir>    fetch from Hugging Face
llmops freeze   <hf_repo>@<sha> --dir <dir>         write the store manifest in place
llmops push     <hf_repo>@<sha> --dir <dir> --bucket <root>
llmops verify   <prefix>                            check a store against its manifest
llmops list     --bucket <root>                     what is mirrored there
llmops serve    --manifest <manifest.yaml>          run a model
llmops validate <models-dir | manifest.yaml>        check manifests and deploys
llmops install  --manifest <manifest.yaml>          place the unit + manifest on a host
llmops bench    --url <base> --model <id>           measure a live endpoint
llmops version
```

Freeze a model into S3 (one-time per revision). `--bucket` takes any
s5cmd-reachable root, so AWS S3, DO Spaces, R2 and MinIO all work:

```sh
llmops pull moonshotai/Kimi-K2.7-Code --dir /scratch/kimi
llmops push moonshotai/Kimi-K2.7-Code@<sha> \
    --dir /scratch/kimi --bucket s3://<your-bucket>
llmops verify s3://<your-bucket>/moonshotai/Kimi-K2.7-Code/<sha>/
```

A host that serves from its own disk needs no bucket at all — `freeze`
writes the same checksummed manifest in place, and `load: local` verifies
it before every start:

```sh
llmops pull   Qwen/Qwen3.8-27B@<sha> --dir ~/.models/Qwen/Qwen3.8-27B/<sha>
llmops freeze Qwen/Qwen3.8-27B@<sha> --dir ~/.models/Qwen/Qwen3.8-27B/<sha>
```

Validate manifests, then serve. `llmops serve` is both the container
entrypoint and what the systemd unit runs, so the two deploy modes start
the same process:

```sh
llmops validate models/
llmops serve --manifest /etc/llmops/model.yaml --cache-root ~/.models
```

On a bare-metal host, `install` places the manifest and a generated
systemd unit, then systemd runs the same `serve` the container does:

```sh
sudo llmops install --manifest models/<name>.yaml --cache-root ~/.models
sudo systemctl enable --now <name>.service
```

Each model endpoint speaks OpenAI Chat natively (engine passthrough)
and Anthropic Messages at `/anthropic/v1/messages` via the shared
[`latere.ai/x/pkg/llmdialect`](https://github.com/latere-ai/pkg)
translator; the Lux dialect is served by Lux itself, which embeds the
same package.

Benchmark a live endpoint:

```sh
llmops bench --url http://kimi-k2-7-code.llmops.svc:8000 \
    --model kimi-k2.7-code --concurrency 8 --requests 32 --out report.json
```

## Testing

```sh
make test        # go vet + go test ./...
make cover       # same, with a ≥90% total-statement coverage gate
make validate    # every models/*.yaml plus the deploy artifact it owns
make e2e         # pull→push→verify and serve→ready→metrics
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

Working today: the `llmops` command end to end — weight fetch, freeze
and verify, the serving entrypoint and health shim, the bench harness,
and `install` for a bare-metal host. Pinned manifests for all seven
models with the deploy artifact each one owns, and a consistency check
between them that runs in CI and in `llmops validate`. The whole pipeline
is exercised end to end on a laptop.

Qwen3.8-27B's weights are frozen on a GB10 host and its manifest and unit
are checked in; the endpoint is not up yet.

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
