---
title: Lux Integration (access plane)
status: draft
depends_on:
  - 003-serving-runtime.md
affects:
  - deploy/
effort: small
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Lux Integration (access plane)

## Overview

llmops endpoints become first-class providers in Lux, the latere
model gateway. Lux owns authn (virtual keys), usage/cost tracking,
limits, and audit; llmops exposes the model endpoint. The goal state: a
latere app switches from `openrouter/deepseek/...` to
`llmops/deepseek-v4-pro` by changing a model string in Lux — nothing
else.

Two things narrow this spec's boundary since it was written.

**It is one of two ways in, not the way in.** A coding harness points
straight at a host with no gateway in the path
([[026-harness-integration]]). That path is for a machine someone owns
and works on; Lux is for anything with more than one caller, and it
remains the only ingress for a fleet endpoint.

**Dialect translation happens on both sides of it.** An llmops endpoint
serves all three caller dialects itself ([[025-dialect-surfaces]]), and
Lux embeds the same `llmdialect` package, so the gateway and the endpoint
behind it cannot disagree about what a request means. What Lux must
**not** find here is its own dialect: that surface belongs to the
gateway.

## Scope

1. **Provider registration**: each served model registered in Lux as a
   provider/route pointing at its base URL — in-cluster service DNS
   (`http://<model>.llmops.svc:8000/v1`) for a `deploy: k8s` model, and
   `http://<host>:<port>/v1` for a `deploy: bare-metal` one, which has no
   service DNS at all ([[020-bare-metal-packaging]]). Mechanism follows
   Lux's existing provider config (investigate at impl time whether
   that's DB config, yaml, or dashboard — reuse, don't invent).
2. **Cost model**: per-model amortized $/token entered into Lux's cost
   tracking so router-vs-self-host economics stay visible (inputs: node
   cost, measured tok/s from 010 benchmarks).
3. **Naming**: model ids stable and versioned: `llmops/<name>` with
   the manifest revision surfaced in Lux metadata.

## Acceptance criteria

1. The first served model is callable through Lux with a virtual key
   from outside the cluster; usage rows appear with correct token counts
   (e2e test in the gateway's own harness or a script here, decide at
   impl). Written as Kimi-K2.7-Code, on the roadmap order at the time.
   The model that actually serves first is Qwen3.8-27B on a bare-metal
   host ([[022-model-qwen3.8-27b]]), so it is the cheapest first
   registration and exercises the no-service-DNS path in scope 1.
2. Streaming works end-to-end through Lux (SSE passthrough verified).
3. Tool-calls round-trip through Lux unmodified (e2e).
4. Cost per 1M tokens configured and visible in the Lux dashboard for
   each live model.

## Non-goals

- Failover routing between self-hosted and router providers (Lux feature,
  tracked there if wanted).
- Public internet exposure of engine pods (Lux is the only ingress).

## Verification

- e2e criteria 1–3 against a live model; screenshots/output recorded in
  Outcome.
