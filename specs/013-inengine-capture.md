---
title: In-engine jspace capture (vLLM plugin + SGLang patch)
status: draft
depends_on:
  - 003-serving-runtime.md
  - 012-lens-fitting-pipeline.md
affects:
  - lens/
  - internal/manifest/
  - internal/runtime/
  - Dockerfile.sglang
  - Dockerfile.vllm
effort: large
created: 2026-07-22
updated: 2026-07-22
author: changkun
dispatched_task_id: null
---

# In-engine jspace capture (vLLM plugin + SGLang patch)

## Overview

Real-time jspace monitoring without a second model copy: forward hooks
**inside the engine worker** capture the residual stream at monitored
layers during decode, apply the fitted lens ([[012-lens-fitting-pipeline]])
**on-GPU**, and publish only top-k readouts per (token, layer) to an
on-node socket. Coverage is **all decode tokens** (last position only)
— per token per monitored layer the cost is one rank-r projection +
unembed top-k (a few GEMVs), and the transfer off-GPU is bytes, so the
serving path stays hot.

Both engines ship from day one: a shared capture core with two thin
adapters. The residual stream is replicated across TP ranks
(Megatron-style TP), so capture runs on **rank 0 only**; no cross-GPU
gather.

## Components

1. **Capture core** (`lens/capture/`, engine-agnostic) —
   - `LensApplier`: loads the `_lens/` artifact from the local weight
     dir, holds `U_l`/`V_l` on the worker GPU, and per hook fire
     computes `topk(unembed(U_l @ (V_l @ h_last)))` in-place.
   - Frame publisher: non-blocking ring buffer drained by a thread
     writing NDJSON frames to a unix domain socket
     (`/run/openllms/jspace.sock`):
     `{rid, seq, token_id, layer, topk_ids, topk_logits, ts}`.
     **Backpressure drops frames, never blocks the forward pass.**
2. **vLLM adapter** — a vLLM general plugin (entry-point
   `vllm.general_plugins`; no fork) that registers post-init forward
   hooks on the selected decoder layers of the rank-0 worker.
3. **SGLang adapter** — SGLang has no plugin API: a patch module
   (installed in `Dockerfile.sglang`, activated via
   `sitecustomize`/env) wraps the model runner after load and
   registers the same hooks. Pinned to the image's SGLang version;
   a version-mismatch check refuses to activate silently.
4. **Manifest config** ([[003-serving-runtime]] schema extension) —

   ```yaml
   lens:
     enabled: true
     rank: 256            # must match a fitted artifact
     topk: 10
     layers: from-artifact  # or explicit list
   ```

   `runtime serve` renders this to `OPENLLMS_LENS_*` env vars for the
   engine process and fails readiness if `enabled: true` but no
   `_lens/` artifact was synced.
5. **Request correlation** — the shim assigns/propagates
   `X-Request-Id` to the engine; adapters tag frames with the engine's
   request id so [[014-jspace-readout-api]] can join frames to API
   requests.

## Performance budget

- ≤2% decode-throughput regression with 8 monitored layers, k=10
  (bench harness from [[010-observability-bench]] asserts this).
- VRAM: ≤50 MB/layer at r=256 (factors + workspace) on rank 0.
- Zero overhead when `lens.enabled` is false or absent (hooks never
  registered).

## Acceptance criteria

1. Unit: hook math equals reference lens application (upstream jlens
   `apply` on the same fixture) within fp tolerance, on a tiny torch
   model.
2. vLLM CPU-mode e2e: Qwen3-0.6B + the [[012-lens-fitting-pipeline]]
   artifact serves a streamed chat completion while frames for every
   decode token × monitored layer arrive on the socket, correctly
   tagged with the request id. (Note: the mlx_lm path in e2e/local
   cannot host torch hooks — capture e2e uses vLLM CPU.)
3. SGLang e2e: same assertion on a GPU runner (release gate, not PR
   gate, per [[003-serving-runtime]] precedent).
4. Slow/absent socket consumer: frames drop, token throughput
   unaffected (test with a stalled reader).
5. TP=2 test (vLLM, 2 small GPUs or mocked ranks): frames come from
   rank 0 only, no duplicates.
6. Bench: ≤2% tok/s regression vs `lens.enabled: false` on the same
   model/hardware.
7. Manifest validation: `lens.rank` without a matching artifact fails
   `/ready`; unknown `lens` fields rejected (unit tests).

## Non-goals

- Prefill/prompt-position capture (decode last-position only; prefill
  sampling is a future extension).
- Readout transport beyond the node-local socket
  ([[014-jspace-readout-api]]).
- Fleet MoE lens artifacts ([[016-bigmodel-lens-fitting]]).

## Verification

- CI: unit + vLLM CPU e2e per PR; SGLang GPU e2e + bench as release
  gates via `make e2e`.
