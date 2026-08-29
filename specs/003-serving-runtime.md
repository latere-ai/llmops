---
title: Serving Runtime (engine container + S3 loading + API surface)
status: draft
depends_on:
  - 001-inference-engine-selection.md
  - 002-weights-registry.md
affects:
  - runtime/
  - models/
  - Dockerfile.sglang
  - Dockerfile.vllm
effort: medium
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# Serving Runtime (engine container + S3 loading + API surface)

## Overview

One containerized runtime pattern that turns a `models/<name>.yaml`
manifest into a serving process: fetch/stream weights from the frozen S3
prefix, launch the pinned engine (SGLang or vLLM) with the manifest's
args, and expose the OpenAI-compatible API plus the latere health/metrics
contract (`/healthz`, `/ready`, `/metrics`), the same one the other
latere serving images honor.

Engines already ship OpenAI-compatible servers — we do **not** reshape
the inference path, unlike a domain service that adds its own task
endpoints on top. The runtime is: entrypoint + weight loading + config
rendering + health surface. Thin by design.

**System prompts.** An optional manifest block

```yaml
system_prompt:
  mode: default | prepend | override
  text: "..."
```

is enforced by the shim on every chat request, on both dialect surfaces:
`default` fills in only when the caller sends no system message,
`prepend` always inserts before the caller's, `override` replaces it.
This is the inference layer's knob for deployment-wide behavioral
defaults (and a stable prefix helps RadixAttention cache hit rates);
per-tenant/policy prompts stay in Lux. Task prompts stay in the domain
wrappers that own the task.

**Dialects.** The engine's OpenAI Chat surface proxies through
unchanged. The shim additionally serves `POST /v1/messages`
(Anthropic Messages, streaming included) by translating through the
shared `latere.ai/x/pkg/llmdialect` package (anthropic frontend →
openaichat backend) — so Claude-style callers can hit a model endpoint
directly. The **Lux dialect is deliberately not served here**: that
surface belongs to the gateway, which embeds the same package
([[009-lux-integration]]); duplicating it per pod would re-implement
gateway concerns.

## Components

1. **`runtime/entrypoint`** — reads `MODEL_MANIFEST` (mounted yaml),
   resolves the S3 prefix, prepares weights (below), renders engine CLI
   args, execs the engine server. One image per engine
   (`Dockerfile.sglang`, `Dockerfile.vllm`), engine versions pinned per
   [[001-inference-engine-selection]].
2. **Weight preparation**, two modes selected in the manifest:
   - `load: nvme-cache` (default, both engines): sync S3 prefix →
     node-local NVMe path via `s5cmd` (verify against `_manifest.json`),
     then point the engine at the local dir. Idempotent: verified files
     are not re-fetched; concurrent pods on one node share the cache
     (flock).
   - `load: s3-stream` (vLLM only): `--load-format runai_streamer`
     directly from the S3 URI, concurrency 16–32. No disk staging.
3. **Health surface** — engines expose their own health endpoints; a tiny
   sidecar-free shim maps them to the latere contract: `/healthz` (process
   up), `/ready` (model loaded, engine reports ready), `/metrics`
   (engine's Prometheus output passed through, plus weight-load duration
   gauge).

## Manifest schema (`models/<name>.yaml`)

```yaml
name: kimi-k2.7-code
hf_repo: moonshotai/Kimi-K2.7-Code
revision: <sha>                 # pinned; matches S3 prefix
s3_prefix: s3://latere-models/moonshotai/Kimi-K2.7-Code/<sha>/
format: int4-qat
license: modified-mit           # + note field for clauses
runtime: sglang                 # sglang | vllm | custom
# image: ghcr.io/latere-ai/...  # required iff runtime: custom
load: nvme-cache
gpu: { type: h200, count: 8, nodes: 1 }
context_max: 262144
args:                           # engine-specific flags, verbatim
  - --tp-size=8
  - --tool-call-parser=kimi_k2
  - --reasoning-parser=kimi_k2
```

Schema validated by a `runtime validate` subcommand (test-covered); CI
validates all manifests.

## Custom runtimes (`runtime: custom`)

The two engine images cover OpenAI-chat-shaped models. Anything else —
OCR wrappers, TTS/ASR beyond what vLLM serves, embeddings, future
modalities — plugs in as `runtime: custom` with an
`image:` reference. The contract is the same one the engine images honor:
weights arrive from the manifest's S3 prefix (nvme-cache mode), the
manifest is mounted at `MODEL_MANIFEST`, and the container exposes
`/healthz`, `/ready`, `/metrics`. Everything downstream (k8s deploy, Lux
registration, dashboards) is runtime-blind. Custom images live in their
own repos; this repo only validates the manifest and deploys the
container.

## Acceptance criteria

1. `docker run` with a small open model manifest (e.g. a <10 GB model
   mirrored by [[002-weights-registry]]'s test) serves
   `/v1/chat/completions` from S3-frozen weights on a single local GPU —
   the e2e test for the whole S3→engine path, runnable in CI on one GPU.
2. `/healthz`, `/ready`, `/metrics` behave per the shared service
   contract: `/ready` 503 during load, 200 after; metrics include
   `weights_load_seconds`.
3. NVMe cache mode: second pod start on a warm node skips download
   (test asserts no S3 GETs); corrupted cache file is detected and
   re-fetched (test).
4. `s3-stream` mode boots the same test model on vLLM with zero disk
   staging.
5. Manifest validation rejects unknown fields, unpinned revisions, and
   engine/args mismatches; `runtime: custom` without `image` is rejected
   (unit tests).
6. `POST /v1/messages` returns a well-formed Anthropic
   Messages response (and SSE stream) backed by the engine's OpenAI
   endpoint; malformed requests 400, engine failures pass through
   (tests against a fake engine).

## Non-goals

- Custom inference endpoints or prompt logic (engines' OpenAI API is the
  surface; Lux adds policy).
- Multi-node launch (leader/worker wiring lives in [[008-k8s-serving]]).
- Autoscaling, PD-disaggregation, KV-cache tiering.

## Verification

- CI: manifest validation + shim unit tests; single-GPU e2e with the test
  model (needs a GPU runner — if unavailable, e2e runs via `make e2e` on a
  GPU node and is a release gate, not a PR gate).
