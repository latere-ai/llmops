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

1. **Live inspector** — a single self-contained HTML page embedded in
   the shim binary (`go:embed`, `internal/runtime/jspaceui/index.html`)
   and served at `GET /jspace/ui`. Flow: fetch `/jspace/requests`,
   pick a rid, subscribe to `/jspace/stream?rid=`, and fill a
   **layer × step grid** in real time — columns are decode steps
   (labeled with the sampled token), rows are monitored layers, each
   cell shows the layer's lens top-1 token colored by the rank of the
   finally-sampled token in that layer's top-k (the visual grammar of
   upstream jacobian-lens's slice viz). Clicking a cell shows the full
   top-k + watchlist masses from the frame. No external assets (pods
   have no egress guarantee), no build step — vanilla JS in one file.
2. **Grafana dashboard** — `deploy/dashboards/jspace.json`, panels
   over the four [[014-jspace-readout-api]] metric families:
   entropy-by-layer heatmap, lens/final agreement depth profile,
   watchlist mass per model, frame-drop rate. Provisioned by the
   observability stack from [[010-observability-bench]].

## Acceptance criteria

1. Inspector renders a correct grid from a recorded frame sequence
   (golden test: feed fixture frames through a fake `jspaceHub`,
   assert the generated DOM/table state — no browser dependency in
   CI; a manual pass against the vLLM CPU e2e stack is documented in
   the runbook).
2. The page makes no requests outside `/jspace/*` (test scans the
   HTML for external URLs; runtime CSP header `default-src 'self'`).
3. `/jspace/ui` with lens disabled renders the explicit "lens
   disabled" state (no errors, no retry storm).
4. Dashboard JSON passes schema lint in CI and every panel query
   references one of the four exported metric families by exact name.

## Non-goals

- Historical replay/storage (in-memory ring only).
- Alert rules (follow-up once baseline ranges are known from real
  traffic).
- Multi-request comparison views.

## Verification

- CI: golden DOM test + external-URL scan + dashboard schema lint per
  PR; manual inspector check against `make e2e-local` + capture stack
  in the runbook.
