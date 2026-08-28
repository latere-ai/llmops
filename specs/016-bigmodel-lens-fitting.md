---
title: Lens fitting for fleet MoE models (hardware-gated)
status: draft
depends_on:
  - 012-lens-fitting-pipeline.md
  - 008-k8s-serving.md
affects:
  - lens/
  - deploy/
effort: large
created: 2026-07-22
updated: 2026-07-22
author: changkun
dispatched_task_id: null
---

# Lens fitting for fleet MoE models (hardware-gated)

## Overview

[[012-lens-fitting-pipeline]] proves the pipeline on small dense
models. Fitting the fleet models (`models/kimi-k2.7-code.yaml`,
`models/glm-5.2.yaml`, `models/minimax-m3.yaml`,
`models/deepseek-v4-pro.yaml` — 1T-class MoE on 8×H200) is a
different problem: the estimator needs **backward passes (VJPs)**
through the full model — training-scale memory on models we only ever
serve, and the serving engines don't even ship a training stack. This
spec is the gate: cost it, pick a strategy, and only then spend GPU
time. Deliberately last — like [[007-model-deepseek-v4-pro]] it is
hardware-gated and must not block the monitoring plane for models
where fitting is affordable.

## Components

1. **Cost model** (paper study, no GPUs) — per fleet model: VRAM for
   a VJP at the model's TP/EP sharding with activation checkpointing,
   wall-clock for the ~100-prompt minimum corpus, node-hours. The
   deliverable is a table **in this spec**, updated in place. Note
   the folded-factor step (`A_l = W_U @ U_l`,
   [[012-lens-fitting-pipeline]] §2) needs the unembedding at fit
   time only — at V≈160k, r=256 the factor is ~80 MB/layer, so
   serving VRAM is unchanged from the small-model case; the open
   question is purely backward-pass memory (`J_l` itself at d≈7k is
   ~100 MB fp16 — not the constraint; the activation graph is).
2. **Memory strategies**, evaluated in cost-table order:
   - **Layer-subset fitting**: only monitored layers (stride 4–8)
     need `J_l`; cotangent accumulation stops early per source layer.
   - **Low-rank-first accumulation**: accumulate in sketched/factored
     form so no strategy ever materializes more than the checkpointed
     backward graph requires.
   - **Frozen-expert approximation**: VJP through dense components +
     top-k routed experts only; readout degradation quantified on a
     small MoE reference before acceptance (criterion 3).
3. **Fit job** — a k8s Job in `deploy/` running a dedicated fit image
   (torch + transformers + `llmops-jlens` + `s5cmd`; **not** the
   serving images — vLLM/SGLang lack the training stack). Weights
   arrive by the same `s5cmd sync` + `_manifest.json` verify pattern
   as `internal/runtime/prep.go`; artifacts upload via `jlens upload`
   and are immediately consumable by [[013-inengine-capture]]'s
   `PrepareLens` with zero downstream changes.
4. **Go/no-go per model** — a fleet model gets a lens only if:
   (a) fit cost ≤ the agreed node-hour budget, (b) `jlens verify`
   gates pass, (c) bench shows [[013-inengine-capture]]'s ≤2%
   overhead budget still holds at fleet batch sizes.

## Acceptance criteria

1. Cost table for all four fleet models with cited memory math and a
   recommendation per model (fit / approximate / skip).
2. First fitted fleet model (cheapest per the table) produces an
   artifact passing `jlens verify`; the capture e2e on its serving
   deploy streams jspace frames.
3. If the frozen-expert approximation is used: measured top-k overlap
   vs a dense-VJP reference on a small MoE (<30B) ≥ agreed threshold
   before applying to fleet models.
4. Fit job resumability: a killed job loses at most one checkpoint
   interval (test on the small MoE, reusing
   [[012-lens-fitting-pipeline]]'s checkpointing).

## Non-goals

- New lens math beyond the listed approximations.
- Continuous re-fitting (weights are frozen; one lens per
  model+revision).
- Multi-node fitting for DeepSeek-V4-Pro if it exceeds one node —
  deferred until the single-node models are done.

## Verification

- Phase 1 (cost model) is a doc-only deliverable reviewed in this
  spec. Phase 2 verified by the small-MoE reference tests + the first
  fleet artifact's verify/capture e2e.
