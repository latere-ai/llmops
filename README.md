# open-llms

Latere's end-to-end inference layer for open-weights models. Instead of
renting model access through a router (OpenRouter et al.), this repo owns
the full path: weights frozen in our own S3 bucket, inference engines
running on bare-metal Kubernetes GPU nodes, OpenAI-compatible endpoints
registered behind Lux, the Latere model gateway.

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

## Usage

Freeze a model into S3 (one-time per revision):

```sh
go run ./cmd/mirror pull moonshotai/Kimi-K2.7-Code --dir /scratch/kimi
go run ./cmd/mirror push moonshotai/Kimi-K2.7-Code@<sha> \
    --dir /scratch/kimi --bucket s3://latere-models
go run ./cmd/mirror verify s3://latere-models/moonshotai/Kimi-K2.7-Code/<sha>/
```

Validate manifests and serve (inside the runtime image on a GPU node):

```sh
go run ./cmd/runtime validate models/
runtime serve --manifest /etc/openllms/model.yaml   # container entrypoint
```

Each model endpoint speaks OpenAI Chat natively (engine passthrough)
and Anthropic Messages at `/anthropic/v1/messages` via the shared
[`latere.ai/x/pkg/llmdialect`](https://github.com/latere-ai/pkg)
translator; the Lux dialect is served by Lux itself, which embeds the
same package.

Benchmark a live endpoint:

```sh
go run ./cmd/bench --url http://kimi-k2-7-code.open-llms.svc:8000 \
    --model kimi-k2.7-code --concurrency 8 --requests 32 --out report.json
```

`make cover` runs the test suite with a ≥90% coverage gate; the CI-run
e2e suite exercises mirror pull→push→verify and the runtime
serve→ready→metrics path against fakes. GPU serving on real hardware is
a release gate per model (specs/004–007).

## Status

Implementation of specs 001–003 and 008/010 scaffolding is in place
(mirror tool, runtime entrypoint + health shim, manifests with pinned
revisions, LWS deploys, bench harness). Not yet done: the actual
multi-hundred-GB mirrors, GPU deployments, and Lux registration
(specs/004–007, 009). Read `specs/README.md` for the roadmap and
`specs/001-inference-engine-selection.md` for the engine decision.
