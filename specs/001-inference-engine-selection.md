---
title: Inference Engine Selection (decision record)
status: complete
depends_on:
  - 000-architecture.md
affects:
  - README.md
  - models/
effort: small
created: 2026-07-18
updated: 2026-07-18
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
default:

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
  Not vendor-listed by any of our four models.
- **NVIDIA Dynamo / llm-d / AIBrix now** — orchestrators, not engines;
  all presuppose an engine underneath. Premature before we outgrow
  single-node-per-model. llm-d is CNCF sandbox (young); Dynamo brings
  etcd/NATS weight we don't need yet. Revisit at multi-node PD-disagg
  scale ([[008-k8s-serving]] Future).
- **Modular MAX** — MoE support immature + Qualcomm acquisition (June
  2026) roadmap risk. **LMDeploy** — no trillion-scale story.
  **KTransformers** — CPU+GPU offload niche; keep in mind for budget
  experiments only.

## Version pins (initial)

| Engine | Version | Rationale |
|---|---|---|
| SGLang | v0.5.15.post1 | Covers all four models (K2.7 ≥0.5.10, GLM-5.2 ≥0.5.13.post1, V4 ≥0.5.12) |
| vLLM | v0.25.1 | GLM-5.2/V4 hardened (≥0.23); M3 via `vllm/vllm-openai:minimax-m3` image only |

Pins live in [[003-serving-runtime]] Dockerfiles; bump deliberately, never
track latest.

## Acceptance criteria

1. This record reflects reality at decision time with sources (done above).
2. `models/*.yaml` schema has a `runtime` field; nothing outside the
   model manifest + runtime image encodes an engine choice.
3. Revisit trigger documented: revisit if (a) a target model gains an
   engine-exclusive capability we need, (b) InferenceX shows a sustained
   >30% throughput gap on our workload, or (c) we scale past single-node
   and need PD-disagg orchestration.

## Non-goals

- Benchmarking engines ourselves before first deployment (use InferenceX;
  our own bench harness comes in 010-observability-bench).
