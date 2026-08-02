---
title: "Model: DeepSeek-V4-Pro (largest; hardware-gated)"
status: draft
depends_on:
  - 004-model-kimi-k2.7-code.md
affects:
  - models/deepseek-v4-pro.yaml
  - deploy/deepseek-v4-pro/
effort: large
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# Model: DeepSeek-V4-Pro (largest; hardware-gated)

## Overview

Last in the roadmap because it is hardware-gated: 1.6T MoE (49B active),
release checkpoint FP4 experts + FP8 rest at 864.7 GB — **does not fit
8x H100 (640 GB)**. MIT license. Options, in preference order:

1. **8x B200/B300 single node** — native FP4 path (SGLang "MegaMoE" is
   Blackwell-only); best perf/W.
2. **8x H200 single node** — 1,128 GB, tight; reduced context headroom.
3. **2x 8x H100 multi-node** — needs RoCEv2 + NCCL wiring and multi-node
   LWS (first real use of [[008-k8s-serving]]'s multi-node path).

The choice is a procurement question (../terraform); this spec blocks on
it and records the decision.

## Facts (verified 2026-07-18)

- HF: `deepseek-ai/DeepSeek-V4-Pro` (864.7 GB). Context 1M native;
  vendor recommends ≥384K window for "Think Max" mode. Hybrid CSA+HCA
  attention, sparse MLA.
- Engines: SGLang day-0 (≥0.5.12, primary), vLLM ≥0.23 hardened; vLLM
  needs ≥32 GB shared memory in pod spec.
- `-DSpark` variant (+draft module, 892.8 GB) exists for speculative
  decoding — Future, not initial scope.
- V4-Flash (159.6 GB, 13B active) is the cheap fallback if Pro hardware
  slips — decision point recorded below.

## Acceptance criteria

1. Hardware decision recorded here (which of the three shapes; or
   explicit deferral with V4-Flash substituted as an interim model —
   then a `models/deepseek-v4-flash.yaml` with its own smaller shape).
   **Answered 2026-08-02: deferred.** The V4-Flash branch is taken and
   lives in [[017-model-deepseek-v4-flash-0731]] — the 0731 refresh
   (304B/13B, 166.9 GB, bundled DSpark draft head), on the b200 pool
   this spec had earmarked for preference #1. Pro's own hardware
   decision stays open; AC2 (mirror the weights anyway) still stands.
2. `deepseek-ai/DeepSeek-V4-Pro` mirrored + verified in S3 (mirror now
   regardless of hardware timing — freezing weights is the point of this
   repo).
3. Manifest validates; serves on the chosen shape; if multi-node, LWS
   leader/worker + NCCL config documented and cold/warm start measured.
4. e2e suite (`make e2e-deepseek`): chat, streaming, tool calls, think
   modes, ≥384K-context request.
5. Registered in Lux; baseline benchmark recorded and compared against
   published V4 numbers (lmsys.org/blog/2026-04-25-deepseek-v4).

## Non-goals

- DSpark speculative decoding; PD-disaggregation at 100+ GPU scale
  (vLLM recipes exist — Future); Base checkpoints (post-training).

## Verification

- e2e green on the chosen shape; Outcome records the hardware decision
  path and numbers.
