---
title: Observability and Benchmark Harness
status: partial
depends_on:
  - 003-serving-runtime.md
affects:
  - internal/bench/
  - cmd/llmops/
  - deploy/
effort: small
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Observability and Benchmark Harness

## Overview

Two thin deliverables: (a) engine metrics landing in the cluster
Prometheus with a per-model dashboard, (b) a repeatable benchmark harness
producing the numbers the per-model specs and Lux cost entries require.

## Scope

1. **Metrics**: engines' native Prometheus output (both SGLang and vLLM
   export request rates, TTFT, queue depth, KV-cache utilization) scraped
   via PodMonitor ([[008-k8s-serving]]); one Grafana dashboard per model
   from a shared template: tok/s, TTFT p50/p95, concurrent requests,
   KV-cache %, GPU utilization (DCGM), plus the shim's own
   `llmops_weights_load_seconds`, `llmops_dialect_loss_total{dialect,field}`
   ([[025-dialect-surfaces]]) and `llmops_speculator_info`
   ([[027-qwen-fast-path]]).

   **Device-memory metrics are absent on the GB10 class**
   ([[019-gb10-serving-target]] AC8). `nvidia-smi` reports no per-process
   device memory there, because CPU and GPU share one pool; DCGM
   utilization panels will be empty on that pool. **Host** memory is the
   substitute signal, and the engine's own KV-cache gauges are the
   substitute for cache occupancy. A dashboard for a gb10 model must not
   present an empty DCGM panel as a healthy zero.
2. **Bench harness** (`internal/bench`, `llmops bench`): drives
   OpenAI-compatible endpoints with configurable concurrency and output
   length; outputs a JSON report.

   The original preference — wrap `vllm bench serve` /
   `sglang.bench_serving` / `genai-perf` rather than write a load
   generator — was **not taken**. Each of those ships with one engine's
   Python environment, and the point of measuring is to compare across
   engines, deploy modes and a router from a machine that may have
   neither engine installed. A few hundred lines of Go in the binary that
   already exists costs less than a Python dependency at bench time.

   What the report carries today: `requests`, `errors`, `chunks`,
   `duration_s`, `ttft_p50_ms`, `ttft_p95_ms`, `chunks_per_s`, plus the
   config it ran. Two gaps against the original list — **tok/s per GPU**
   (chunks are not tokens, and the report knows nothing about the GPU
   count) and **ITL** — both wanted before the numbers feed a Lux cost
   entry in AC3.
3. **Router comparison mode**: same harness pointed at the equivalent
   OpenRouter model to produce the self-host-vs-router latency/cost
   table that justifies this repo.

## Acceptance criteria

1. Dashboard template renders for any model manifest; live for the first
   deployed model.
2. `llmops bench --url <base> --model <id>` produces a stable JSON
   report; two consecutive runs within documented variance (test).
   **Built, and it has measured a live model**
   ([[022-model-qwen3.8-27b]]); the documented-variance half is not
   recorded anywhere.
3. Numbers flow into: per-model spec Outcome sections, Lux cost config
   ([[009-lux-integration]]).
4. Router comparison report for at least one model checked into the repo.

## Non-goals

- Quality/accuracy evals (task-level evaluation is owned elsewhere).
- Alerting/SLOs (later, once real traffic exists).

## Verification

- CI: harness unit tests + report-format golden test; live runs are
  release gates per model.
