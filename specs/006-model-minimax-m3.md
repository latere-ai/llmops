---
title: "Model: MiniMax-M3 (MXFP8, sparse attention)"
status: draft
depends_on:
  - 004-model-kimi-k2.7-code.md
affects:
  - models/minimax-m3.yaml
  - deploy/minimax-m3/
effort: medium
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# Model: MiniMax-M3 (MXFP8, sparse attention)

## Overview

Third model; smallest of the four (428B MoE, 23B active) but carries two
sharp edges: MiniMax Sparse Attention (MSA) with a **mandatory
`--block-size 128`** on every platform, and a commercial-use license that
requires action before production.

## Facts (verified 2026-07-18)

- HF: `MiniMaxAI/MiniMax-M3-MXFP8` (443.8 GB) — serving checkpoint; BF16
  (854.2 GB) deferred. Context 1M; native multimodal (text/image/video).
- MSA: 9x prefill / 15x decode speedup vs M2 at 1M context.
- Shape: 8x H200 (BF16 tight single-node; MXFP8 runs from TP4 — evaluate
  TP4 on 4 GPUs vs TP8 for throughput/cost).
- Engines: SGLang (primary), vLLM **not in a stable release** — dedicated
  `vllm/vllm-openai:minimax-m3` Docker image only (re-check at impl
  time). Parsers: `minimax_m3` (tool-call + reasoning).
- **License gate: MiniMax Community License.** Commercial use under $20M
  annual revenue requires a one-time notice to api@minimax.io and "Built
  with MiniMax M3" display. Send the notice and record it in the manifest
  **before** Lux exposure; >$20M revenue would need written authorization.

## Acceptance criteria

0. License notice sent and evidenced in `models/minimax-m3.yaml`
   (blocking gate for AC5).
1. `MiniMaxAI/MiniMax-M3-MXFP8` mirrored + verified in S3.
2. Manifest validates; `--block-size 128` enforced by manifest validation
   for this model (test: manifest without it is rejected).
3. Serves via LWS; TP4-vs-TP8 measured once, decision recorded.
4. e2e suite (`make e2e-minimax`): chat, streaming, tool calls, reasoning
   parsing, one image-input request (multimodal), long-context request.
5. Registered in Lux; baseline benchmark recorded.

## Non-goals

- Video input; NVFP4/Blackwell and AMD MXFP4 paths; BF16 checkpoint.

## Verification

- e2e green; Outcome records TP decision, license-notice evidence,
  benchmark numbers.
