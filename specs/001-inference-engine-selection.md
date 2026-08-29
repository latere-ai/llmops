---
title: Inference Engine Selection (decision record)
status: complete
depends_on:
  - 000-architecture.md
affects:
  - README.md
  - models/
  - Dockerfile.sglang
  - Dockerfile.vllm
effort: small
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Inference Engine Selection (decision record)

## Decision

**SGLang is the primary engine; vLLM is the supported second engine; the
runtime stays engine-agnostic** (each `models/<name>.yaml` declares
`runtime: sglang|vllm|custom` plus engine args; `custom` admits non-chat
model classes — see [[003-serving-runtime]]). No orchestrator initially — plain
LWS single-node deployments ([[008-k8s-serving]]); adopt llm-d or Dynamo
only when we need prefill/decode disaggregation at multi-node scale.

## Why SGLang primary (state of the world, 2026-07-18)

For the 500B–1T MoE class we target, SGLang is the mid-2026 community
default. (Four models were in scope when this was written; the registry
is seven now, and the update below says what that changed.)

- Every trillion-scale reference deployment published by a model lab runs
  on it: Kimi K2 on 128x H200 with PD-disaggregation + wide-EP (224k tok/s
  prefill / 288k tok/s decode cluster-wide, lmsys.org/blog/2025-07-20),
  DeepSeek-V4 day-0 (lmsys.org/blog/2026-04-25), GB200 NVL72 records.
- First-to-land kernel work for exactly our models: MLA/DSA sparse
  attention, native FP4 experts (DeepSeek-V4), GLM-5.2 NVFP4 tuning,
  MiniMax-M3 support. All four target models have vendor-listed SGLang
  support; MiniMax-M3's vLLM support is Docker-image-only (not in a stable
  release) as of today.
- RadixAttention prefix caching (75–95% hit rates on multi-turn/agentic
  workloads) fits our expected traffic (coding agents, long system
  prompts).
- Production adoption: xAI, Cursor, LinkedIn, Azure, Oracle; 400k+ GPUs
  claimed.

## Why keep vLLM as second engine

- Broadest hardware/model matrix and largest contributor base; the safer
  fallback when SGLang breaks on a release (both projects leapfrog weekly
  — SemiAnalysis InferenceX daily CI is the living benchmark:
  https://inferencex.semianalysis.com/).
- First-class S3 weight streaming (`--load-format runai_streamer`), which
  SGLang lacks — this shapes [[003-serving-runtime]]: the NVMe prefetch
  path must work for both engines, S3-direct streaming is a vLLM-only
  bonus.
- An existing latere serving stack already runs vLLM; operational
  familiarity.
- Moonshot lists vLLM first for Kimi; DeepSeek-V4 vLLM support is
  production-hardened since v0.23.

## Rejected

- **TensorRT-LLM standalone** — classic compile-engine backend deprecated;
  survives as kernels (FlashInfer, DSA-in-SGLang) and a Dynamo backend.
  Not vendor-listed by any model in the registry.
- **NVIDIA Dynamo / llm-d / AIBrix now** — orchestrators, not engines;
  all presuppose an engine underneath. Premature before we outgrow
  single-node-per-model. llm-d is CNCF sandbox (young); Dynamo brings
  etcd/NATS weight we don't need yet. Revisit at multi-node PD-disagg
  scale ([[008-k8s-serving]] Future).
- **Modular MAX** — MoE support immature + Qualcomm acquisition (June
  2026) roadmap risk. **LMDeploy** — no trillion-scale story.
  **KTransformers** — CPU+GPU offload niche; keep in mind for budget
  experiments only.

## Version pins

Three images, not two: Kimi-K3 ships in a CUDA 13 branch build, and the
shared SGLang image cannot substitute for it in either direction.

| Image | Pin | Serves | Floor that set it |
|---|---|---|---|
| `Dockerfile.sglang` default | `lmsysorg/sglang:v0.5.16-cu129` | K2.7-Code, GLM-5.2, MiniMax-M3, V4-Pro, V4-Flash-0731 | V4-Flash DSpark ≥0.5.16 ([[017-model-deepseek-v4-flash-0731]]) |
| `Dockerfile.sglang` K3 build | `lmsysorg/sglang:kimi-k3` (CUDA 13, r580+ driver) | Kimi-K3 only | [[018-model-kimi-k3]] AC4 |
| `Dockerfile.vllm` | `vllm/vllm-openai:v0.28.0` | Qwen3.8-27B | sm_120 kernels for GB10 ([[019-gb10-serving-target]]) |

Pins live in the Dockerfiles ([[003-serving-runtime]]); bump
deliberately, never track latest. The first SGLang pin was
`v0.5.15.post1-cu126`, a tag that does not exist upstream at all — a pin
nobody has pulled is not yet a pin.

## Update (2026-08-29): the second engine is the one in production

Nothing above is reversed, but two things read differently now.

**vLLM serves the only live endpoint.** Qwen3.8-27B runs on it
([[022-model-qwen3.8-27b]]), because a dense 27B on one GB10 is not the
model class this record was written about. SGLang stays primary for all
six MoE models; "second engine" describes the fallback role for the
fleet, not the share of endpoints running today.

**A new hardware class did not need a new engine.** GB10 is arm64 with a
single unified-memory GPU. Both pinned images publish `linux/arm64`, and
vLLM's sm_120 kernels run on the sm_121 device by binary compatibility —
measured, not assumed ([[019-gb10-serving-target]]). The decision
survived the first hardware class it did not anticipate.

None of the three revisit triggers in AC3 has fired.

## Acceptance criteria

1. This record reflects reality at decision time with sources (done above).
2. `models/*.yaml` schema has a `runtime` field; nothing outside the
   model manifest + runtime image encodes an engine choice. Holds:
   `runtime` selects the image (`image` is required by, and permitted
   only for, `custom`), and [[025-dialect-surfaces]] added
   `engine_dialect` beside it, so which dialect an engine speaks
   natively is a manifest fact rather than shim wiring.
3. Revisit trigger documented: revisit if (a) a target model gains an
   engine-exclusive capability we need, (b) InferenceX shows a sustained
   >30% throughput gap on our workload, or (c) we scale past single-node
   and need PD-disagg orchestration.

## Non-goals

- Benchmarking engines ourselves before first deployment (use InferenceX;
  our own bench harness comes in 010-observability-bench).
