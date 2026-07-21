---
title: Jacobian lens fitting pipeline (per-model lens artifacts)
status: draft
depends_on:
  - 002-weights-registry.md
  - 011-local-e2e.md
affects:
  - lens/
  - e2e/local/
effort: medium
created: 2026-07-22
updated: 2026-07-22
author: changkun
dispatched_task_id: null
---

# Jacobian lens fitting pipeline (per-model lens artifacts)

## Overview

First Python code in this repo: a package (`lens/`, distributed as
`openllms-jlens`) that fits a **Jacobian lens** for a model+revision and
publishes the result as a versioned artifact next to the frozen weights
in S3. The lens is the offline half of real-time jspace monitoring
([[013-inengine-capture]] is the online half): a per-layer linear map

```
lens_l(h) = unembed(J_l @ h),   J_l = E[∂h_final / ∂h_l]
```

that transports residual-stream activations at layer *l* into the
final-layer basis so the unembedding can decode what the model is
"lined up to say" mid-computation. Reference: Anthropic's
[jacobian-lens](https://github.com/anthropics/jacobian-lens) (Apache
2.0, reference implementation of "Verbalizable Representations Form a
Global Workspace in Language Models"). We reimplement the estimator
(cotangents summed over target positions, averaged over source
positions, over a small prompt corpus) in our package rather than
depending on the unmaintained repo; a converter imports jlens-fitted
`.pt` files.

Fitting cost is dominated by backward passes and saturates at
~100–1000 prompts. This spec covers small/mid dense models only —
enough to prove the whole pipeline on the [[011-local-e2e]] stack.
Fleet-scale MoE fitting is [[016-bigmodel-lens-fitting]].

## Components

1. **Estimator** (`lens/fitting/`) — accumulates `J_l` per monitored
   layer via VJPs on a prompt corpus. Layer selection is a stride/list
   (default: every 4th layer + final). Deterministic given corpus +
   seed; supports fitting disjoint corpus slices and `merge()`ing.
2. **Low-rank truncation** — at save time each `J_l` is
   SVD-truncated to rank *r* (default 256): stored as `U_l (d×r)`,
   `V_l (r×d)`. This is what makes serving-time VRAM affordable
   (10–40 MB/layer instead of `d²`). Full-rank save is a debug flag.
3. **Artifact format** — `safetensors` (no pickle in serving) plus
   metadata:

   ```
   s3://<s3_prefix>/_lens/
     lens-r256.safetensors        # U_l, V_l per layer
     lens.json                    # model, revision, layers, rank,
                                  # d_model, corpus sha256, fit config,
                                  # per-tensor sha256, package version
   ```

   The artifact lives under the model's frozen `s3_prefix`, so
   [[003-serving-runtime]]'s existing weight prep (s5cmd sync +
   hash verification) fetches it with zero new machinery.
4. **CLI** — `jlens fit --model <hf-or-local-dir> --prompts <file>
   --layers stride:4 --rank 256 --out <dir>`; `jlens merge`;
   `jlens verify <dir>`; `jlens upload --manifest models/<name>.yaml`.
5. **Verify** — sanity gates before upload: (a) final-layer lens
   top-1 agrees with model logits on ≥95% of held-out positions
   (final-layer Jacobian ≈ identity); (b) mid-layer readouts are
   non-degenerate (entropy within bounds, not collapsed to one token).

## Acceptance criteria

1. `jlens fit` on Qwen3-0.6B (the [[011-local-e2e]] model) completes
   on a laptop (CPU/MPS) in minutes on a 100-prompt corpus and
   produces a loadable artifact.
2. `jlens verify` passes on that artifact; a corrupted tensor is
   detected via `lens.json` hashes (test).
3. Merging two disjoint 50-prompt fits equals a single 100-prompt fit
   within tolerance (test).
4. `jlens upload` places the artifact under the manifest's
   `s3_prefix/_lens/` in MinIO; e2e/local asserts the runtime's weight
   sync fetches it alongside weights.
5. Converter loads an upstream jacobian-lens `.pt` and re-saves in our
   format; `verify` passes (test with a tiny fixture).
6. Unit coverage ≥90% for `lens/fitting/`.

## Non-goals

- Serving-time capture/apply ([[013-inengine-capture]]).
- Fitting fleet MoE models ([[016-bigmodel-lens-fitting]]).
- Lens quality research (rank/corpus ablations beyond the verify
  gates).

## Verification

- CI: unit tests + the Qwen3-0.6B fit as part of `e2e/local` (CPU,
  zero-cost). Artifact determinism asserted by hash across two runs
  with the same seed.
