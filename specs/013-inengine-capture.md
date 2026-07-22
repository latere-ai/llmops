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
**inside the engine worker** read the residual stream at monitored
layers during decode, apply the folded lens factors
([[012-lens-fitting-pipeline]]) **on-GPU**, and publish only top-k
readouts per (request, step, layer) to a node-local socket the shim
owns ([[014-jspace-readout-api]]). Coverage is **all decode tokens**.

Decode steps are batched: a hook fires once per step per layer with
the residual state `h` of shape `[B, d]` (one row per in-flight
request; decode rows are last-position by construction — prefill
steps are skipped by checking the engine's forward mode). Lens apply
is two GEMMs + a top-k:

```
scores = A_l @ (V_l @ h.T)      # [V, B]; r×(d+V) MACs per row
topk_l = topk(scores, k)        # per row
```

With folded factors this is ~0.5% of a decode step's FLOPs at B=64,
8 layers, r=256 — the transfer off-GPU is bytes per token. The
residual stream is replicated across TP ranks (Megatron-style), so
capture runs on **rank 0 only**; no cross-GPU gather.

Both engines ship from day one: one capture core, two thin adapters,
pinned to the image versions from [[001-inference-engine-selection]]
(vLLM `v0.25.1` in `Dockerfile.vllm`, SGLang `v0.5.15.post1` in
`Dockerfile.sglang`).

## Components

1. **Capture core** (`lens/src/openllms_jlens/capture/`,
   engine-agnostic) —
   - `LensApplier`: loads `A_l`/`V_l` from the local `_lens/` dir
     (path via `OPENLLMS_LENS_DIR`), keeps them on the rank-0 GPU,
     computes top-k per batch row per monitored layer.
   - **Watchlist mass** is computed here, not in the Go shim (the shim
     has no tokenizer): watchlists arrive as
     `OPENLLMS_LENS_WATCHLISTS` (JSON `{name: [strings]}`), are
     tokenized against the model dir's tokenizer (first-token ids for
     multi-token entries), and per-list softmax mass is computed over
     the **full** lens distribution — no top-k truncation.
   - Frame publisher: per-row NDJSON frames

     ```json
     {"rid": "...", "seq": 42, "token_id": 1234, "layer": 20,
      "topk_ids": [...], "topk_logits": [...],
      "watch": {"refusal": 0.03}, "ts": 1690000000.0}
     ```

     `token_id` is the token the engine **sampled** at that step
     (taken from the sampler output in the same step — the forward
     hook alone cannot know it), so [[014-jspace-readout-api]] can
     compute lens/final agreement without joining streams. A
     non-blocking ring buffer is drained by a writer thread dialing
     the shim's unix socket (`OPENLLMS_LENS_SOCK`, default
     `/run/openllms/jspace.sock`) with reconnect+backoff.
     **Backpressure drops frames (counted), never blocks the forward
     pass.**
2. **vLLM adapter** — a general plugin (entry-point group
   `vllm.general_plugins` declared in `lens/pyproject.toml`; no fork)
   that registers post-init forward hooks on the selected decoder
   layers of the rank-0 worker. Row→request mapping comes from the
   scheduler's per-step request ids (the model-runner input batch
   carries request ids in step order); sampled ids from the same
   step's sampler output. Exact symbols verified against `v0.25.1` at
   implementation time.
3. **SGLang adapter** — SGLang has no plugin API: a patch module
   installed in `Dockerfile.sglang` (activated via `sitecustomize`
   when `OPENLLMS_LENS_ENABLED=1`) wraps the model runner after load
   and registers the same hooks; row→request mapping from the
   schedule batch's per-request `rid`s. The patch asserts
   `sglang.__version__` equals the pinned version and otherwise
   **fails engine startup loudly** — never silently no-ops.
4. **Request-id contract** — the shim mints an id per chat request
   and returns it to the caller as the `X-Request-Id` response header
   ([[014-jspace-readout-api]]); on the engine leg it propagates via
   the mechanism each adapter documents (vLLM: `X-Request-Id`
   header → engine request id; SGLang: body `rid` extension on the
   intercepted request). Frames carry whatever id the engine saw;
   the shim owns the mapping.
5. **Manifest extension** (`internal/manifest/manifest.go`; note
   `Parse` uses `KnownFields(true)`, so the struct must gain the
   field) —

   ```yaml
   lens:
     enabled: true
     rank: 256              # must match the fitted artifact
     topk: 10               # 1..100
     watchlists:            # optional; name -> string list
       refusal: ["I'm sorry", "cannot"]
   ```

   `Validate()`: `rank>0`, `topk` in range, non-empty watchlist
   entries; `lens` on `runtime: custom` rejected.
6. **Runtime wiring** (`internal/runtime/serve.go`, `prep.go`) —
   - `PrepareLens(m, dir, store, log)`: when `lens.enabled`, fetch
     `_lens/lens.json` + tensors into the weight cache dir, verifying
     per-tensor sha256 from `lens.json` (same `ensureFile` pattern;
     `_lens/` files are deliberately absent from the frozen
     `_manifest.json`, see [[012-lens-fitting-pipeline]]). Missing or
     rank-mismatched artifact fails `Serve` **before** engine launch.
   - `Serve` sets `cmd.Env = append(os.Environ(),
     OPENLLMS_LENS_ENABLED, _DIR, _TOPK, _SOCK, _WATCHLISTS...)` on
     the engine process.

## Performance budget

- ≤2% decode-throughput regression with 8 monitored layers, k=10
  (asserted with the [[010-observability-bench]] harness).
- VRAM on rank 0: ≤100 MB/layer at r=256 (folded factors + workspace).
- `lens.enabled` false/absent ⇒ hooks never registered, zero
  overhead, no env vars set.

## Acceptance criteria

1. Unit (pytest, tiny torch fixture): hook math equals reference
   jacobian-lens `apply` within fp tolerance; watchlist mass equals a
   numpy reference.
2. vLLM CPU e2e (Linux container; macOS dev runs the unit fixture —
   mlx_lm cannot host torch hooks): Qwen3-0.6B + the fitted artifact
   serves a streamed chat completion while frames for every decode
   token × monitored layer arrive on the socket with the correct rid
   and the actually-sampled `token_id`.
3. SGLang e2e: same assertion on a GPU runner (release gate, not PR
   gate, per [[003-serving-runtime]] precedent).
4. Stalled/absent socket consumer: frames drop with the drop counter
   rising; token throughput unaffected (test).
5. TP=2 (vLLM, 2 GPUs or mocked ranks): frames from rank 0 only, no
   duplicates.
6. SGLang version mismatch: engine startup fails with an explicit
   error (test against a fake version string).
7. Go side: `PrepareLens` fetch/verify/corruption-refetch tests;
   manifest validation tests (unknown lens fields, bad rank/topk,
   lens+custom); `Serve` env rendering test via the
   `OPENLLMS_ENGINE_CMD` override.
8. Bench: ≤2% tok/s regression vs `lens.enabled: false`, same
   model/hardware.

## Non-goals

- Prefill/prompt-position capture (decode-only; prefill sampling is a
  future extension).
- Readout transport beyond the node-local socket, aggregation, UI
  ([[014-jspace-readout-api]], [[015-jspace-dashboard]]).
- Fleet MoE lens artifacts ([[016-bigmodel-lens-fitting]]).

## Verification

- CI: pytest units + Go units per PR; vLLM CPU e2e per PR (dockerized,
  no GPU); SGLang GPU e2e + bench as release gates via `make e2e`.
