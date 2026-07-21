---
title: jspace readout API (shim surface + metrics)
status: draft
depends_on:
  - 013-inengine-capture.md
affects:
  - internal/runtime/
  - internal/manifest/
effort: medium
created: 2026-07-22
updated: 2026-07-22
author: changkun
dispatched_task_id: null
---

# jspace readout API (shim surface + metrics)

## Overview

The Go shim ([[003-serving-runtime]]) consumes capture frames from the
node-local socket ([[013-inengine-capture]]) and exposes them two ways:
a **live per-request stream** for interactive inspection, and
**Prometheus aggregates** for fleet-wide monitoring/alerting. This is
the layer that turns raw (token, layer, top-k) frames into "monitor
jspace content/intent in real time".

The shim already owns request ids and the `/metrics` merge point, so
no new process is added: a reader goroutine drains the socket into a
bounded in-memory ring (per active request) plus streaming aggregators.

## Components

1. **Frame ingest** — shim connects to `/run/openllms/jspace.sock`,
   parses NDJSON frames, joins on request id. Bounded buffers
   (per-request ring of last N tokens, default 512); overflow drops
   oldest. Ingest failure degrades to "no jspace data", never affects
   proxying.
2. **Live stream** — `GET /jspace/stream?rid=<id>` (SSE): frames for
   one in-flight/recent request, fanned out to any number of
   subscribers. `GET /jspace/requests` lists recent rids with model +
   timestamps. Callers obtain the rid from the `X-Request-Id` response
   header on their chat completion.
3. **Aggregates on `/metrics`** (appended alongside
   `openllms_weights_load_seconds`):
   - `openllms_jspace_layer_entropy{layer}` — rolling mean entropy of
     the top-k readout per layer (intent-formation depth profile).
   - `openllms_jspace_lens_final_agreement{layer}` — fraction of
     tokens where the layer's top-1 equals the final sampled token
     (how early the output is "decided").
   - `openllms_jspace_watchlist_mass{layer,list}` — probability mass
     on configured token watchlists (e.g. refusal markers, tool-call
     openers) per layer.
   - `openllms_jspace_frames_dropped_total` — ingest/capture drops.
4. **Watchlists** — manifest extension:

   ```yaml
   lens:
     watchlists:
       refusal: ["I'm sorry", "cannot", ...]   # tokenized at load
   ```

5. **Access control** — `/jspace/*` is an operator surface: bound to
   the same listener but excluded from Lux registration
   ([[009-lux-integration]]); k8s NetworkPolicy scopes it to the
   monitoring namespace.

## Acceptance criteria

1. Fake-capture unit tests: frames in → SSE out, ordered per rid;
   two concurrent subscribers get identical streams.
2. e2e (vLLM CPU stack from [[013-inengine-capture]]): a streamed chat
   completion's rid yields live SSE frames while tokens are still
   generating; `/jspace/requests` lists it.
3. `/metrics` exposes all four metric families with correct values on
   a synthetic frame sequence (golden test).
4. Watchlist tokenization: multi-token entries aggregate first-token
   mass; unknown tokens rejected at manifest validation (unit tests).
5. Socket absent / capture disabled: all `/jspace/*` endpoints return
   an explicit "lens disabled" state; proxying unaffected (test).
6. Ring overflow under a 10k-token request drops oldest frames without
   unbounded memory (test asserts bound).

## Non-goals

- Cross-node aggregation and alert rules (Grafana/alerting config is
  [[015-jspace-dashboard]]).
- Persistence of frames beyond the in-memory ring (transcript
  archival is a future spec if needed).
- AuthN/AuthZ beyond network scoping (gateway concern).

## Verification

- CI: unit + golden metric tests per PR; the [[013-inengine-capture]]
  vLLM CPU e2e extended with SSE + metrics assertions.
