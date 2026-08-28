---
title: llmops Architecture (umbrella)
status: draft
depends_on: []
affects:
  - README.md
  - specs/
effort: large
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# llmops Architecture (umbrella)

## Overview

Own the inference layer for open-weights models end to end, replacing
router-mediated access (OpenRouter) for the models we care about:

- **MiniMax-M3** (428B MoE, 23B active, 1M ctx)
- **GLM-5.2** (753B MoE, ~40B active, 1M ctx, MIT)
- **Kimi-K2.7-Code** (1T MoE, 32B active, native INT4, 256K ctx)
- **DeepSeek-V4-Pro** (1.6T MoE, 49B active, FP4+FP8, 1M ctx, MIT)
- **Qwen3.8-27B** (27B **dense**, multimodal, BF16, 262K ctx, Apache-2.0)

The last one is deliberately unlike the others, and the registry is not
only for frontier-scale MoE: it is what we choose to own. A model small
enough to run undamaged on one GPU is a different product from a huge
one squeezed to fit, not a lesser version of it ([[022-model-qwen3.8-27b]]).

Three planes:

1. **Weights plane** — frozen, checksummed mirrors, in our S3 bucket or
   on a host's own disk ([[002-weights-registry]],
   [[021-local-weight-loading]]). Never re-download from HF. The freeze
   guarantee is a pinned revision plus per-file checksums, not the
   storage behind them.
2. **Serving plane** — inference engine(s) exposing OpenAI- and
   Anthropic-compatible APIs, in either of two deploy modes: k8s on
   multi-GPU nodes loading from S3, or an installed binary under systemd
   on a single-GPU host ([[001-inference-engine-selection]],
   [[003-serving-runtime]], [[008-k8s-serving]],
   [[020-bare-metal-packaging]]). Both run the same `llmops serve` and
   share the manifest schema, so a model's description does not depend
   on how it is started.
3. **Access plane** — endpoints registered in Lux, the latere model
   gateway, which owns authn, keys, usage, and cost
   ([[009-lux-integration]]). llmops does **not** re-implement gateway
   concerns.

## Constraints

- The MoE targets are very large: the minimum serving unit is an
  8x H200-class node (Kimi INT4, GLM FP8, MiniMax MXFP8); DeepSeek-V4-Pro
  wants 8x B200/B300 for its native FP4 path or 2x8 H100 multi-node.
  Hardware acquisition is out of scope here (infrastructure provisioning)
  but specs must state each model's exact GPU footprint.
- The single-GPU class is a different shape, not a smaller one: one GPU,
  one memory pool shared with the CPU, and no cluster around it. Its
  constraint is memory *behaviour* rather than capacity — an engine's
  memory fraction is taken from the host's RAM too
  ([[019-gb10-serving-target]]).
- License hygiene per model: GLM-5.2 and DeepSeek-V4 are MIT; Kimi-K2.7 is
  modified-MIT (attribution clause); MiniMax-M3 is MiniMax Community
  License (commercial notice required; >$20M revenue needs written
  authorization); Qwen3.8-27B is Apache-2.0, the least encumbered of the
  set. Recorded per model in `models/*.yaml`.
- The shared latere service conventions apply: `/healthz`, `/ready`,
  `/metrics` contract, Docker + k8s packaging, e2e-verified features.

## Spec roadmap (ordered, each tightly scoped)

| # | Spec | Delivers |
|---|---|---|
| 001 | [[001-inference-engine-selection]] | Engine decision record (vLLM vs SGLang vs Dynamo/llm-d) |
| 002 | [[002-weights-registry]] | HF→S3 mirror tool, manifests, initial 2.7 TB mirror |
| 003 | [[003-serving-runtime]] | One containerized engine runtime: S3 streaming load, OpenAI API, health/metrics |
| 004 | 004-model-kimi-k2.7-code | First model live end to end (smallest footprint, single node) |
| 005 | 005-model-glm-5.2 | Second model (FP8 + expert parallel path) |
| 006 | 006-model-minimax-m3 | Third model (MXFP8, sparse attention flags) |
| 007 | 007-model-deepseek-v4-pro | Fourth model (largest; may need multi-node or Blackwell) |
| 008 | [[008-k8s-serving]] | LWS-based GPU deployment, NVMe prefetch cache, gang scheduling |
| 009 | 009-lux-integration | Models registered/routable through Lux with usage accounting |
| 010 | 010-observability-bench | Engine metrics in Prometheus, latency/throughput benchmark harness |

Order rationale: decision (001) and weights (002) unblock everything;
Kimi-K2.7-Code goes first because native INT4 on one 8x H200 node is the
cheapest full path to prove the S3→engine→Lux pipeline; DeepSeek-V4-Pro
goes last because it has the hardest hardware requirement.

## Non-goals

- Post-training (fine-tuning/RL). Frozen BF16/base weights make it
  *possible* later; the training stack belongs to a separate repo. This
  repo's only obligation: don't preclude it, keep base-checkpoint
  mirroring supported in the registry tool.
- Domain API wrappers (OCR endpoints and the like) — those stay their
  own repos. But their *weights* belong in the registry, and their
  containers can deploy here via `runtime: custom`
  ([[003-serving-runtime]]): the repo is scoped to any self-hosted
  open-weights model (OCR, audio, multimodal, embeddings), not only
  frontier chat LLMs.
- Building our own gateway features (Lux owns that).

## Verification

Umbrella is done when specs 001–010 exist, are individually verified, and
at least one model serves production traffic through Lux from S3-frozen
weights.
