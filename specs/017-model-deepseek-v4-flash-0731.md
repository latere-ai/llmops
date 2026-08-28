---
title: "Model: DeepSeek-V4-Flash-0731 (speculative-decoding path)"
status: draft
depends_on:
  - 003-serving-runtime.md
  - 007-model-deepseek-v4-pro.md
  - 008-k8s-serving.md
affects:
  - models/deepseek-v4-flash-0731.yaml
  - deploy/deepseek-v4-flash-0731/
  - internal/manifest/
  - Dockerfile.sglang
effort: medium
created: 2026-08-02
updated: 2026-08-02
author: changkun
dispatched_task_id: null
---

# Model: DeepSeek-V4-Flash-0731 (speculative-decoding path)

## Overview

This is the answer to [[007-model-deepseek-v4-pro]] AC1's escape hatch:
Pro stays hardware-gated, and the Flash refresh gives us a DeepSeek-V4
endpoint now at **1/5 the weight footprint** (166.9 GB vs 864.7 GB) and
1/4 the active parameters. On DeepSeek's own benchmarks it beats
V4-Pro (Preview) across Terminal Bench, NL2Repo, DeepSWE, and
Toolathlon — the coding/agentic axis latere actually serves — so this is
not a downgrade substitution, it is the better model per GPU-hour.

It also brings the first **speculative decoding** into the fleet. The
0731 checkpoint bundles a DSpark draft head in the same safetensors
tree, so `--speculative-algorithm DSPARK` needs no second checkpoint and
no second S3 prefix. That is what makes it cheap to run and what makes
its manifest easy to get wrong — hence the validation rules below.

## Facts (verified 2026-08-02)

- HF: `deepseek-ai/DeepSeek-V4-Flash-0731`,
  revision `7872f01b1d1fe23eabc4c98b48bffcef5a386062`, **166.9 GB**,
  48 safetensors shards. 304B total / 13B active MoE, FP4 experts +
  FP8 attention/dense. Context 1M; vendor recommends ≥384K output budget
  at `high`/`max` reasoning effort. MIT license — no gate.
- Engines: SGLang **≥0.5.16** (the DSpark path is verified end-to-end on
  v0.5.16); vLLM ≥0.27 with `--speculative-config '{"method":"dspark"}'`.
  Our `Dockerfile.sglang` pinned `v0.5.15.post1-cu126` — a tag that does
  not exist upstream at all (404) — so it is bumped to `v0.5.16-cu129`
  as part of this spec.
- Vendor-verified shapes: **8x B200**, 4x GB300, 4x H200.
- Parsers: `deepseek-v4` reasoning, `deepseekv4` tool-call. Reasoning
  modes Non-think / Think High / Think Max via `reasoning_effort`.
- Sampling: `temperature=1.0`, `top_p=0.95` agentic / `1.0` otherwise.
- **No Jinja chat template.** The repo ships an `encoding/` folder
  (`encoding_dsv4.encode_messages`) plus a DSML tool-call grammar
  instead; the engine's own `deepseekv4` parsers cover the
  OpenAI-compatible surface, so the shim needs no change — but a
  template-shaped assumption anywhere downstream will break.
- DSpark constraints (SGLang): requires CUDA, `pp_size == 1`, DP
  attention **disabled**, no `--speculative-draft-model-path`, and is
  incompatible with PD disaggregation. Block size resolves from the
  checkpoint (5 proposed tokens on 0731) unless overridden.

## Hardware decision

**8x B200, single node (TP8), `latere.ai/gpu-pool: b200`.**

Chosen over 4x H200 despite the H200 pool having more slack: the Hopper
SM90 path for DeepSeek-V4 runs an all-FP8 MegaMoE variant configured
through **environment variables** (`SGLANG_OPT_USE_DEEPGEMM_MEGA_MOE`,
`SGLANG_DSV4_FP4_EXPERTS`, …), and the manifest schema carries `args`
only — no `env`. Blackwell needs none of that: `--moe-runner-backend
flashinfer_mxfp4` and the CLI flags below are the whole configuration.
Taking the b200 pool is also exactly what 007 AC1 contemplated, since
Pro is not deployed and remains blocked on procurement.

Adding `env` to the manifest schema would unlock the H200 path and is
worth doing when a second model needs it — Future, not here.

## Acceptance criteria

1. Mirrored to S3 at the pinned revision with `_manifest.json` written
   and `mirror verify` clean ([[002-weights-registry]] AC4).
2. `models/deepseek-v4-flash-0731.yaml` validates: `sglang`, b200 x8 x1,
   TP8, DSpark on, `deepseek-v4`/`deepseekv4` parsers,
   `--trust-remote-code`.
3. Manifest validation **rejects** the three DSpark foot-guns with tests
   that fail without the rule: DSPARK paired with
   `--speculative-draft-model-path`, with `--pp-size` > 1, or with
   `--enable-dp-attention`.
4. Serves on one 8x B200 node via LWS; `/ready` from warm NVMe cache
   recorded, cold recorded separately (expect 10–15 min extra on first
   boot while FlashInfer autotunes and SGLang captures the draft and
   verify CUDA graphs).
5. e2e suite (`make e2e-deepseek-flash`): chat, streaming, tool-call
   round-trip, `reasoning_content` at each of the three effort levels,
   and a ≥384K-context request at `max`.
6. Benchmark recorded twice on the same corpus — with and without
   `--speculative-algorithm DSPARK`, each leg with its own
   `--flush-cache` — reporting P50/P99 TTFT, TPOT, accepted length, and
   total throughput. DSpark stays on only if that comparison earns it.
7. Registered in Lux as `llmops/deepseek-v4-flash-0731`; cost per 1M
   tokens derived from AC6's measured throughput.

## Naming

`deepseek-v4-flash-0731`, not `deepseek-v4-flash`. The dated checkpoint
and the original Flash are genuinely different models — different
parameter counts (304B vs 284B) and different speculative paths (DSpark
vs EAGLE) — and the vendor cookbook treats them as separate variants.
Hiding that behind an undated name would make the revision SHA the only
record of which one is live.

## Non-goals

- PD disaggregation (incompatible with DSpark today).
- `env:` support in the manifest schema, and therefore the H200 shape.
- The original `DeepSeek-V4-Flash` and `-DSpark` checkpoints.
- Retiring [[007-model-deepseek-v4-pro]] — Pro stays open; this spec
  only removes the pressure behind it.

## Verification

- AC3 is a unit-test gate (runs in `make test`); AC2 is `make validate`
  plus `deploycheck`'s repo consistency test.
- AC5 e2e green on the b200 node is the release gate; AC4/AC6 numbers
  land in the Outcome section.
