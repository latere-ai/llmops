# open-llms

Latere's end-to-end inference layer for open-weights models. Instead of
renting model access through a router (OpenRouter et al.), this repo owns
the full path: weights frozen in our own S3 bucket, inference engines
running on bare-metal Kubernetes GPU nodes, OpenAI-compatible endpoints
registered behind [Lux](https://github.com/latere-ai/lux) — the Latere
model gateway.

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

| Model | Vendor | Class |
|---|---|---|
| MiniMax M3 | MiniMax | MoE |
| GLM-5.2 | Zhipu AI | MoE |
| Kimi K2.7 Code | Moonshot AI | MoE |
| DeepSeek V4 Pro | DeepSeek | MoE |

Exact revisions, weight formats, and GPU footprints are pinned per model
in `specs/` and in each model's manifest under `models/`.

## Repo layout

```
specs/      design specs (start at specs/README.md)
models/     per-model manifests: HF revision, S3 prefix, engine config
tools/      weight mirroring (HF → S3), verification, benchmarks
deploy/     k8s manifests for GPU serving
```

## Status

Spec stage. Read `specs/README.md` for the roadmap and
`specs/001-inference-engine-selection.md` for the vLLM vs SGLang
decision record.
