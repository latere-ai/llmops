---
title: Observability and Benchmark Harness
status: partial
depends_on:
  - 003-serving-runtime.md
affects:
  - tools/bench/
  - deploy/
effort: small
created: 2026-07-18
updated: 2026-07-18
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
   KV-cache %, GPU utilization (DCGM), plus `weights_load_seconds` from
   the runtime shim.
2. **Bench harness** (`tools/bench`): drives OpenAI-compatible endpoints
   with configurable concurrency/prompt-length/output-length mixes;
   outputs a JSON report (tok/s per GPU, TTFT, ITL, error rate). Prefer
   wrapping an existing tool (`vllm bench serve` / `sglang.bench_serving`
   / `genai-perf` — pick one at impl time) over writing our own load
   generator; our code is the config, runner, and report format.
3. **Router comparison mode**: same harness pointed at the equivalent
   OpenRouter model to produce the self-host-vs-router latency/cost
   table that justifies this repo.

## Acceptance criteria

1. Dashboard template renders for any model manifest; live for the first
   deployed model.
2. `llmops bench --model kimi-k2.7-code` produces a stable
   JSON report; two consecutive runs within documented variance (test).
3. Numbers flow into: per-model spec Outcome sections, Lux cost config
   ([[009-lux-integration]]).
4. Router comparison report for at least one model checked into the repo.

## Non-goals

- Quality/accuracy evals (task-level evaluation is owned elsewhere).
- Alerting/SLOs (later, once real traffic exists).

## Verification

- CI: harness unit tests + report-format golden test; live runs are
  release gates per model.
