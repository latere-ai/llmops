---
title: "Model: GLM-5.2 (FP8 + expert parallel)"
status: draft
depends_on:
  - 004-model-kimi-k2.7-code.md
affects:
  - models/glm-5.2.yaml
  - deploy/glm-5.2/
effort: medium
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Model: GLM-5.2 (FP8 + expert parallel)

## Overview

Second model; exercises the FP8 + expert-parallel path and 1M context.
753B MoE (~40B active), MIT license — no gate of any kind, unlike
[[006-model-minimax-m3]] and [[018-model-kimi-k3]], whose licenses block
Lux exposure until a notice is sent and a determination recorded. Two
thinking-effort modes (High/Max).

## Facts (verified 2026-07-18)

- HF: `zai-org/GLM-5.2-FP8` (755.7 GB) — the serving checkpoint; BF16
  main repo (1,506.7 GB) deferred to post-training.
- Context 1M. "IndexShare" sparse attention.
- Recommended shape (vendor vLLM recipe): 8x H200, TP8,
  `--enable-expert-parallel --quantization fp8`; 1M context requires
  `--kv-cache-dtype fp8_e5m2` + chunked prefill.
- The manifest carries the SGLang spelling of that recipe, since
  `runtime: sglang`: `--ep-size=8` for expert parallel, and no
  `--quantization` flag because the checkpoint declares FP8 in its own
  config. Chunked prefill is **not** passed explicitly — confirm the
  engine default covers it at 1M context before the first bring-up, and
  record the answer here.
- Engines: SGLang ≥0.5.13.post1 (primary; NVFP4 tuning exists for B300),
  vLLM ≥0.23.

## Acceptance criteria

1. `zai-org/GLM-5.2-FP8` mirrored + verified in S3.
2. Manifest validates against `deploy/glm-5.2/lws.yaml` (`llmops
   validate`, also in CI) — **holds today**; and serves on 8x H200 via
   LWS from NVMe cache, which does not.
3. e2e suite (`make e2e-glm-5.2`, the reserved `e2e-<model>` GPU
   release gate; no such target exists yet): chat, streaming, tool calls, both
   thinking-effort modes, and a long-context test at the largest context
   the deployed KV config admits (documented; full 1M only if fp8 KV
   enabled — record the tradeoff chosen).
4. Registered in Lux; baseline benchmark recorded.

## Non-goals

- BF16 checkpoint serving; Ascend NPU path.

## Verification

- e2e suite green on hardware; Outcome section records context-length
  config and benchmark numbers.
