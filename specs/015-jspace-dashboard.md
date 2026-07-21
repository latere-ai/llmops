---
title: jspace live inspector + dashboards
status: draft
depends_on:
  - 014-jspace-readout-api.md
affects:
  - internal/runtime/
  - deploy/
effort: small
created: 2026-07-22
updated: 2026-07-22
author: changkun
dispatched_task_id: null
---

# jspace live inspector + dashboards

## Overview

Two consumption surfaces for [[014-jspace-readout-api]]:

1. **Live inspector** — a single static HTML page served by the shim
   at `/jspace/ui`: pick a recent request, watch a layer × token grid
   fill in real time from the SSE stream (each cell: the layer's top-1
   token, colored by rank of the final sampled token — the same
   visual grammar as upstream jacobian-lens's slice viz). Self-hosted
   assets only (no CDN; pods have no egress guarantee).
2. **Grafana dashboard** — JSON in `deploy/dashboards/jspace.json`,
   panels over the [[014-jspace-readout-api]] metric families:
   entropy-by-layer heatmap, lens/final agreement depth profile,
   watchlist mass per model, frame-drop rate. Wired into the existing
   observability stack from [[010-observability-bench]].

## Acceptance criteria

1. Inspector renders a live grid against the vLLM CPU e2e stack;
   grid content matches SSE frames (playwright-free check: golden DOM
   snapshot from a recorded frame sequence).
2. Page is a single self-contained file (no external requests — test
   asserts none).
3. Dashboard JSON provisions cleanly on a stock Grafana (schema-lint
   in CI) and every panel query names an existing metric family.
4. Both surfaces degrade gracefully when lens is disabled ("lens
   disabled" state, no errors).

## Non-goals

- Historical replay/storage (in-memory ring only).
- Alert rules (follow-up once baseline metric ranges are known from
  real traffic).
- Multi-request comparison views.

## Verification

- CI: DOM golden test + dashboard schema lint; manual check against
  the local e2e stack documented in the runbook.
