---
title: "Model: Kimi-K2.7-Code (first model live)"
status: draft
depends_on:
  - 002-weights-registry.md
  - 003-serving-runtime.md
  - 008-k8s-serving.md
affects:
  - models/kimi-k2.7-code.yaml
  - deploy/kimi-k2.7-code/
effort: medium
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Model: Kimi-K2.7-Code (first model live)

## Overview

First model through the full pipeline (S3 → runtime → k8s → Lux) because
it has the cheapest full-quality footprint: 1T-param MoE (32B active,
384 experts) shipped as **native quantization-aware-trained INT4** —
595 GB, fits one 8x H200 node (TP8) with ample KV headroom. Coding/agentic
focus matches latere's primary workload. Thinking mode is always on.

## Facts (verified 2026-07-18)

- HF: `moonshotai/Kimi-K2.7-Code`, 595.2 GB, safetensors (INT4 packed as
  I32 + BF16 non-expert tensors). Context 256K.
- License: Modified MIT (attribution clause for very large-scale
  commercial deployment — read LICENSE in repo before production; record
  conclusion in the manifest).
- Engines: SGLang ≥0.5.10 (primary), vLLM ≥0.19.1. Required parsers:
  `kimi_k2` (tool-call + reasoning). vLLM vision-encoder note:
  `--mm-encoder-tp-mode data`. The shared SGLang image is pinned well
  above this floor (`v0.5.16-cu129`, [[001-inference-engine-selection]]),
  so nothing here gates on an engine bump.
- Vendor deploy guide: `docs/deploy_guidance.md` in the HF repo.

## Acceptance criteria

1. Mirrored to S3 with verified manifest ([[002-weights-registry]] AC4).
2. `models/kimi-k2.7-code.yaml` validates; `runtime: sglang`, TP8,
   `--tool-call-parser kimi_k2 --reasoning-parser kimi_k2`, and
   `deploy/kimi-k2.7-code/lws.yaml` matches it — `llmops validate`
   checks manifest and deploy artifact against each other, and CI runs
   the same check. **Holds today**; it is the only criterion here that
   does.
3. Serves on one 8x H200 node via LWS; `/ready` from warm NVMe cache in
   documented time (record cold vs warm numbers).
4. e2e test suite (runnable via `make e2e-kimi-k2.7-code`, the
   `e2e-<model>` GPU release gate the Makefile reserves — no such target
   exists yet): chat completion, streaming, tool call round-trip,
   reasoning-content parsing, 128K-token long-context request. All
   green. The name carries the full model id because
   [[018-model-kimi-k3]] made "kimi" ambiguous.
5. Registered in Lux and reachable with a Lux virtual key
   ([[009-lux-integration]] provides the mechanism; this spec provides the
   entry).
6. Baseline benchmark recorded (010 harness): tok/s at 1/8/32 concurrent,
   TTFT, per-node throughput — compared against published K2-class
   numbers for sanity.

## Non-goals

- Multi-node scale-out; KTransformers CPU+GPU budget path.

## Verification

- AC4 e2e suite is the gate; numbers from AC3/AC6 recorded in the spec's
  Outcome section.
