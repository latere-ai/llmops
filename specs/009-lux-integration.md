---
title: Lux Integration (access plane)
status: draft
depends_on:
  - 003-serving-runtime.md
affects:
  - deploy/
effort: small
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# Lux Integration (access plane)

## Overview

open-llms endpoints become first-class providers in Lux, the latere
model gateway. Lux owns authn (virtual keys), usage/cost tracking,
limits, and audit; open-llms exposes plain in-cluster OpenAI-compatible
services. The goal state: a latere app switches from
`openrouter/deepseek/...` to `open-llms/deepseek-v4-pro` by changing a
model string in Lux — nothing else.

## Scope

1. **Provider registration**: each served model registered in Lux as a
   provider/route pointing at the in-cluster service DNS
   (`http://<model>.open-llms.svc:8000/v1`). Mechanism follows Lux's
   existing provider config (investigate at impl time whether that's DB
   config, yaml, or dashboard — reuse, don't invent).
2. **Cost model**: per-model amortized $/token entered into Lux's cost
   tracking so router-vs-self-host economics stay visible (inputs: node
   cost, measured tok/s from 010 benchmarks).
3. **Naming**: model ids stable and versioned: `open-llms/<name>` with
   the manifest revision surfaced in Lux metadata.

## Acceptance criteria

1. Kimi-K2.7-Code callable through Lux with a virtual key from outside
   the cluster; usage rows appear with correct token counts (e2e test in
   the gateway's own harness or a script here, decide at impl).
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
