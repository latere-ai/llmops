---
title: jspace readout API (shim surface + metrics)
status: draft
depends_on:
  - 013-inengine-capture.md
affects:
  - internal/runtime/
effort: medium
created: 2026-07-22
updated: 2026-07-22
author: changkun
dispatched_task_id: null
---

# jspace readout API (shim surface + metrics)

## Overview

The Go shim (`internal/runtime/shim.go`) consumes capture frames from
the node-local socket ([[013-inengine-capture]]) and exposes them two
ways: a **live per-request SSE stream** for interactive inspection and
**Prometheus aggregates** on the existing `/metrics` merge point. No
new process: the shim already owns the listener, the request path, and
`metrics()`.

The shim is the **server** side of the socket: it listens on
`LLMOPS_LENS_SOCK` before the engine starts (mirroring how `Serve`
brings up `/healthz` before weights load), and the capture publisher
dials in. Ingest failure or a missing publisher degrades to "no
jspace data" — proxying is never affected.

## Components

1. **Request ids** — the shim mints a rid for every
   `POST /v1/chat/completions` (and `/v1/messages`) and
   returns it as the `X-Request-Id` response header. On the proxy
   path this is a `proxy.Director`/`ModifyResponse` pair; on the
   intercepted paths (`chatCompletions`, `anthropicMessages`) it is
   set explicitly. Engine-leg propagation is per-adapter
   ([[013-inengine-capture]] §4); the shim keeps the rid↔engine-id
   mapping so frames join regardless of mechanism.
2. **Frame ingest** (`internal/runtime/jspace.go`, new) — a
   `jspaceHub` accepts socket connections, decodes NDJSON frames, and
   maintains:
   - per-rid ring buffer (last 512 frames; overflow drops oldest,
     bounded memory);
   - per-rid subscriber fan-out (channels; slow subscribers dropped,
     not blocking);
   - streaming aggregators feeding the metric families below.
   A new `Shim.jspace *jspaceHub` field; `nil` when lens is disabled.
3. **HTTP surface** (new `ServeHTTP` cases) —
   - `GET /jspace/stream?rid=<id>` — SSE; replays the rid's ring then
     follows live frames until the request completes. Uses the
     existing `flushWriter` for per-event flushing.
   - `GET /jspace/requests` — recent rids (model, start time, frame
     count) from the ring index.
   - Both return `409 lens disabled` when `jspace == nil`.
4. **Metric families** (appended in `metrics()` beside
   `llmops_weights_load_seconds`, same text-format pattern) —
   - `llmops_jspace_layer_entropy{layer}` — rolling mean entropy of
     the renormalized top-k readout per layer (intent-formation depth
     profile; top-k truncation noted in HELP text).
   - `llmops_jspace_lens_final_agreement{layer}` — fraction of
     frames whose lens top-1 equals the frame's sampled `token_id`
     (how early the output is "decided"; frames carry the sampled
     token per [[013-inengine-capture]] §1).
   - `llmops_jspace_watchlist_mass{layer,list}` — rolling mean of
     the `watch` map (full-softmax mass, computed capture-side).
   - `llmops_jspace_frames_dropped_total` — hub-side drops;
     capture-side drops arrive as a counter frame and are added.
5. **Access control** — `/jspace/*` is an operator surface: excluded
   from Lux registration ([[009-lux-integration]]); k8s NetworkPolicy
   scopes it to the monitoring namespace ([[008-k8s-serving]] deploy
   overlay).

## Acceptance criteria

1. Fake-publisher unit tests: frames in → SSE out, ordered per rid;
   two concurrent subscribers receive identical streams; late
   subscriber gets the ring replay.
2. Every chat response (proxied and intercepted paths, streaming and
   not) carries `X-Request-Id` (httptest against a fake engine).
3. e2e (extends the [[013-inengine-capture]] vLLM CPU run): the rid
   from a streamed completion's response header yields live SSE
   frames while tokens are still generating; `/jspace/requests` lists
   it.
4. `/metrics` exposes all four families with correct values on a
   synthetic frame sequence (golden test), alongside the engine
   passthrough + weights gauge.
5. Lens disabled: `/jspace/*` returns the explicit disabled state;
   `ServeHTTP` proxy behavior byte-identical to today (regression
   test).
6. A 10k-frame rid stays within the 512-frame ring bound; hub memory
   bounded under 100 concurrent rids (test).
7. Publisher disconnect/reconnect mid-request: stream resumes, drop
   counter reflects the gap (test).

## Non-goals

- Cross-node aggregation, alert rules, UI
  ([[015-jspace-dashboard]]).
- Frame persistence beyond the in-memory ring (archival is a future
  spec if needed).
- AuthN/AuthZ beyond network scoping (gateway concern).

## Verification

- CI: unit + golden metric tests per PR (coverage counts toward the
  `make cover` ≥90% gate); the vLLM CPU e2e extended with SSE +
  metrics assertions.
