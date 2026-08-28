---
title: Local Full-Stack E2E (small model, local S3, local engine)
status: complete
depends_on:
  - 002-weights-registry.md
  - 003-serving-runtime.md
affects:
  - e2e/local/
  - internal/runtime/
effort: small
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# Local Full-Stack E2E (small model, local S3, local engine)

## Overview

Before spending on buckets and GPU nodes, prove the entire pipeline with
real components at laptop scale: a real HF model (~1 GB), a real
S3-protocol store (MinIO in Docker — the same wire protocol the
production bucket and `deploy/mirror/job.yaml` will use, exercising
`S3_ENDPOINT_URL` + s5cmd), and a real local engine (`mlx_lm server` on
Apple silicon, OpenAI-compatible) standing in for SGLang via the
runtime's `LLMOPS_ENGINE_CMD` override. The CI fakes prove logic;
this proves the integrations the fakes stub out.

## Scope

`e2e/local/run.sh` drives, and tears down, the full chain:

1. MinIO container up; `latere-models` bucket created.
2. `mirror pull Qwen/Qwen3-0.6B@<pinned sha>` → real HF download,
   SHA256 verification against LFS OIDs.
3. `mirror push` → s5cmd upload to MinIO; `mirror verify` re-hashes
   from the store; `mirror ls` shows the revision.
4. `runtime serve` with `e2e/local/qwen3-0.6b.yaml` (a real manifest:
   pinned revision, `system_prompt` enforced) — weights staged from
   MinIO into a local cache dir, engine launched via
   `LLMOPS_ENGINE_CMD` (mlx_lm), `/ready` flips.
5. Assertions: `/v1/chat/completions` (non-stream + stream) returns
   model output; `/anthropic/v1/messages` returns an Anthropic-shaped
   response; `/metrics` exposes `llmops_weights_load_seconds`;
   system-prompt enforcement observable in output.
   Agentic surface: reasoning content (thinking enabled per-request via
   `chat_template_kwargs`), an OpenAI tool call (`tool_calls` with the
   requested function), a tool-result round trip (answer uses the tool
   output), and Anthropic-format `tools` translating end-to-end into a
   `tool_use` block — the same reasoning/tool paths the production
   models' parsers (`kimi_k2`, `minimax_m3`) serve at scale.
6. `bench` runs a small load and emits a report.
7. Warm-start check: restart serve, `/ready` with zero store reads.

## Engine health-path note

`mlx_lm server` may not expose `/health` (SGLang/vLLM do). The shim's
engine health path becomes overridable via `LLMOPS_ENGINE_HEALTH_PATH`
(default `/health`) — test-covered.

## Acceptance criteria

1. `make e2e-local` runs the chain above green on a clean machine with
   Docker + uv (script bootstraps the venv), no cloud credentials.
2. The manifest used is validated by the same `runtime validate` as
   production manifests (local engine substitution happens via env, not
   schema loosening).
3. Total cost: $0; runtime a few minutes after the one-time model
   download.

## Non-goals

- GPU engines, real buckets, the four production models (specs
  004–007).
- Linux CPU variant (vLLM CPU) — welcome later; script structure
  should not preclude it.

## Verification

The script IS the verification; its output is recorded in this spec's
Outcome section when first run green.

## Outcome (2026-07-18)

First green run on Apple silicon (podman): Qwen3-0.6B (1.5 GB) pulled
from HF and SHA256-verified; pushed via s5cmd to MinIO and re-verified
from the store; cold-start serve staged all 10 files from MinIO in ~6s
and `/ready` flipped once mlx_lm was up; OpenAI chat (thinking disabled
via chat-template args), SSE streaming, Anthropic Messages, and the
metrics gauge all asserted; bench 4 requests / 0 errors; warm restart
re-used the cache with zero store fetches. Findings folded back into
the code: engines now get `--served-model-name <name>` (callers 404'd
otherwise), engine health path is overridable, s5cmd errors carry
stderr, MinIO data must live on host disk (container VM filled up),
and the serve process is exec'd directly (not `go run`) so SIGTERM
reaches the runtime's clean shutdown path.

Agentic extension (same day): reasoning, OpenAI tool call, tool-result
round trip, and Anthropic tool_use translation all asserted green with
Qwen3-0.6B — including the dialect layer mapping Anthropic `tools` /
`tool_use` through the OpenAI engine surface.
