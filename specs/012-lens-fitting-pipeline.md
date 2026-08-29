---
title: Jacobian lens fitting pipeline (per-model lens artifacts)
status: draft
depends_on:
  - 002-weights-registry.md
  - 011-local-e2e.md
affects:
  - lens/
  - e2e/local/
  - Makefile
effort: medium
created: 2026-07-22
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Jacobian lens fitting pipeline (per-model lens artifacts)

## Overview

First Python code in this repo: `lens/` (package `llmops-jlens`,
managed with `uv` like `e2e/local/.venv`) fits a **Jacobian lens** for
a model+revision and publishes it as a versioned artifact next to the
frozen weights in S3. The lens is the offline half of real-time jspace
monitoring ([[013-inengine-capture]] is the online half): a per-layer
linear map

```
lens_l(h) = unembed(J_l @ h),   J_l = E[∂h_final / ∂h_l]
```

that transports residual-stream activations at layer *l* into the
final-layer basis so the unembedding can decode what the model is
"lined up to say" mid-computation. Reference: Anthropic's
[jacobian-lens](https://github.com/anthropics/jacobian-lens) (Apache
2.0; "Verbalizable Representations Form a Global Workspace in Language
Models"). We reimplement the estimator (cotangents summed over target
positions, averaged over source positions and prompts) rather than
depending on the unmaintained repo; a converter imports jlens `.pt`
files.

Fitting cost is backward-pass-dominated and saturates at ~100–1000
prompts. This spec covers small/mid dense models — enough to prove the
pipeline on the [[011-local-e2e]] stack (Qwen3-0.6B @
`c1899de289a04d12100db370d81485cdf75e47ca`). Fleet MoE fitting is
[[016-bigmodel-lens-fitting]].

## Package layout

```
lens/
  pyproject.toml           # uv-managed; python 3.12; entry point: jlens
  src/llmops_jlens/
    fitting.py             # VJP estimator, merge, checkpointing
    artifact.py            # save/load/verify (safetensors + lens.json)
    convert.py             # upstream jacobian-lens .pt importer
    cli.py                 # jlens fit|merge|verify|upload
  tests/                   # pytest; tiny fixture model (2-layer toy)
```

`capture/` is added by [[013-inengine-capture]]; one package, two
subsystems, so fit-time and serve-time code share `artifact.py`.

## Components

1. **Estimator** (`fitting.py`) — accumulates `J_l` per monitored
   layer via VJPs over a prompt corpus (JSONL, one prompt per line;
   corpus sha256 recorded). Layer selection `--layers stride:4`
   (default: every 4th + final) or an explicit list. Deterministic
   given corpus + seed. Supports fitting disjoint corpus slices and
   `merge()`ing (weighted mean by prompt count). Checkpoints
   accumulation every N prompts (`--checkpoint out/ckpt.pt`).
2. **Folded low-rank factors** — at save time each `J_l` is
   SVD-truncated to rank *r* (default 256) and the unembedding is
   folded in:

   ```
   J_l ≈ U_l V_l           # U_l: d×r,  V_l: r×d
   A_l = W_U @ U_l         # V×r  (W_U = tied/lm_head unembedding)
   lens_l(h) = A_l @ (V_l @ h)
   ```

   Storing `(A_l, V_l)` instead of `(U_l, V_l)` is deliberate: the
   serving-side applier ([[013-inengine-capture]]) never has to locate
   the engine's (sharded) `lm_head`, and per-token apply cost drops
   from `d×V` to `r×(d+V)` MACs (~100× at d≈4k, r=256). Size:
   ~80 MB/layer fp16 at V=150k — acceptable next to multi-GB weights.
   `--full-rank` additionally saves raw `J_l` for debugging.
3. **Artifact format** — safetensors (no pickle in serving) +
   self-describing metadata:

   ```
   s3://<s3_prefix>/_lens/
     lens-r256.safetensors    # A_l, V_l per monitored layer
     lens.json                # hf_repo, revision, layers, rank, d_model,
                              # vocab_size, corpus sha256, seed, fit
                              # config, per-tensor sha256, pkg version
   ```

   Note: `_manifest.json` is written at `llmops push` time and is
   frozen — lens files are deliberately **not** added to it. The
   artifact is self-verifying via `lens.json` hashes; the runtime
   fetches it in a separate prep step ([[013-inengine-capture]]'s
   `PrepareLens`, reusing `ensureFile`-style verify from
   `internal/runtime/prep.go`).
4. **CLI** —
   - `jlens fit --model <hf-or-local-dir> --prompts corpus.jsonl
     --layers stride:4 --rank 256 --seed 0 --out <dir>`
   - `jlens merge <dir>... --out <dir>`
   - `jlens verify <dir> --model <dir>` (gates below)
   - `jlens upload <dir> --manifest models/<name>.yaml` (s5cmd-style
     put to `<s3_prefix>/_lens/`; refuses if `lens.json` model/revision
     disagree with the manifest)

   A `load: local` model has **no `s3_prefix` at all**
   ([[021-local-weight-loading]], which landed after this was written),
   so `_lens/` has nowhere to go for the one class of host most likely
   to run a fit locally. Resolve at implementation: put the lens beside
   the weights under the host's `--cache-root`, and let `upload` be the
   S3 case rather than the only case. Every store primitive in
   `internal/mirror` is already store-agnostic, so this is a path
   decision, not a new mechanism.
5. **Verify gates** (run before upload): (a) final-layer lens top-1
   agrees with model logits on ≥95% of held-out positions (final-layer
   Jacobian ≈ identity, so this checks the whole save/fold/load path);
   (b) mid-layer readouts non-degenerate (mean top-k entropy within
   configured bounds, no single-token collapse).

## Acceptance criteria

1. `jlens fit` on Qwen3-0.6B completes on a laptop (CPU/MPS) in
   minutes on a 100-prompt corpus and produces a loadable artifact.
2. `jlens verify` passes on that artifact; a corrupted tensor is
   detected via `lens.json` hashes (test).
3. Merging two disjoint 50-prompt fits equals a single 100-prompt fit
   within tolerance (test); a killed fit resumes from checkpoint
   losing at most N prompts (test).
4. `jlens upload` places the artifact under the manifest's
   `s3_prefix/_lens/` in MinIO (e2e/local extension); model/revision
   mismatch refuses (test).
5. Converter imports an upstream jacobian-lens `.pt` fixture,
   re-saves folded factors, `verify` passes (test).
6. Same corpus + seed ⇒ byte-identical `lens.json` tensor hashes
   across two runs (determinism test).
7. `make test-lens` (pytest + coverage) gates ≥90% for
   `src/llmops_jlens/` fit-side modules, wired into CI beside the Go
   `cover` target.

## Non-goals

- Serving-time capture/apply and `PrepareLens`
  ([[013-inengine-capture]]).
- Fitting fleet MoE models ([[016-bigmodel-lens-fitting]]).
- Lens quality research beyond the verify gates.

## Verification

- CI: `make test-lens` per PR; the Qwen3-0.6B fit + upload runs as a
  step in `e2e/local/run.sh` (CPU, zero-cost), reusing its venv/MinIO
  bootstrap.
