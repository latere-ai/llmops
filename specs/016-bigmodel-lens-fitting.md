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
models. Fitting the fleet models (Kimi-K2.7, GLM-5.2, MiniMax-M3,
DeepSeek-V4-Pro — 1T-class MoE on 8×H200) is a different problem:
the estimator needs **backward passes (VJPs)** through the full model,
i.e. training-scale memory, on models we only ever serve. This spec is
the gate: cost it, pick a strategy, and only then spend GPU time.
Deliberately last — like [[007-model-deepseek-v4-pro]], it is
hardware-gated and must not block the monitoring plane for models
where fitting is affordable.

## Components

1. **Cost model** (paper study, no GPUs) — per fleet model: VRAM for
   VJP with activation checkpointing at TP/EP sharding, wall-clock for
   the ~100-prompt minimum corpus, node-hours. Output: a table in this
   spec, updated in place.
2. **Memory strategies**, evaluated in order of preference:
   - **Layer-subset fitting**: only monitored layers (stride 4–8)
     need `J_l`; cotangent accumulation can stop early per source
     layer.
   - **Low-rank-first accumulation**: accumulate `J_l` directly in
     factored form (sketching) to avoid materializing `d²` in HBM
     (d≈7k ⇒ 100 MB fp16 per layer is fine; the constraint is the
     backward graph, not `J_l` itself — validate this in the cost
     model).
   - **Frozen-expert approximation**: VJP through dense components +
     top-k routed experts only; quantify readout degradation vs a
     dense reference before accepting.
3. **Fit job** — k8s Job spec in `deploy/` reusing the serving image
   base + weight prep from [[003-serving-runtime]]; artifacts upload
   per [[012-lens-fitting-pipeline]] and are immediately consumable by
   [[013-inengine-capture]] with zero changes downstream.
4. **Go/no-go criteria** — a model gets a fitted lens only if:
   (a) fit cost ≤ agreed node-hour budget, (b) `jlens verify` gates
   pass, (c) bench shows the serving overhead budget from
   [[013-inengine-capture]] still holds at fleet scale.

## Acceptance criteria

1. Cost table for all four fleet models with cited memory math, plus a
   recommendation per model (fit / approximate / skip).
2. First fitted fleet model (cheapest by the table) produces an
   artifact passing `jlens verify`; capture e2e on its serving deploy
   streams jspace frames.
3. If the frozen-expert approximation is used: measured top-k overlap
   vs dense-VJP reference on a small MoE (e.g. a <30B MoE) ≥ agreed
   threshold before applying to fleet models.
4. Fit job is resumable (checkpointed accumulation) — a killed job
   loses ≤1 corpus slice (test on the small MoE).

## Non-goals

- New lens math/research beyond the approximations listed.
- Continuous re-fitting (weights are frozen; one lens per
  model+revision).
- Multi-node fitting for DeepSeek-V4-Pro if it exceeds one node —
  explicitly deferred until the single-node models are done.

## Verification

- Phase 1 (cost model) is a doc-only deliverable reviewed in this
  spec. Phase 2 verified by the small-MoE reference tests + the first
  fleet artifact's verify/capture e2e.
