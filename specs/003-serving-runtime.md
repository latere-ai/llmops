---
title: Serving Runtime (engine container + S3 loading + API surface)
status: partial
depends_on:
  - 001-inference-engine-selection.md
  - 002-weights-registry.md
affects:
  - internal/runtime/
  - cmd/llmops/
  - models/
  - Dockerfile.sglang
  - Dockerfile.vllm
effort: medium
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Serving Runtime (engine container + S3 loading + API surface)

## Overview

One runtime pattern that turns a `models/<name>.yaml` manifest into a
serving process: fetch/stream weights from the frozen store, launch the
pinned engine (SGLang or vLLM) with the manifest's args, and expose the
caller-facing API plus the latere health/metrics contract (`/healthz`,
`/ready`, `/metrics`), the same one the other latere serving images
honor.

`llmops serve` is that process, and it is the same process in both deploy
modes: a container entrypoint under Kubernetes ([[008-k8s-serving]]) or
`ExecStart` in a systemd unit on a host with no cluster
([[020-bare-metal-packaging]]). The mode decides how it is started and
supervised, nothing else.

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

is enforced by the shim on every chat request, on all three caller
surfaces:
`default` fills in only when the caller sends no system message,
`prepend` always inserts before the caller's, `override` replaces it.
This is the inference layer's knob for deployment-wide behavioral
defaults (and a stable prefix helps RadixAttention cache hit rates);
per-tenant/policy prompts stay in Lux. Task prompts stay in the domain
wrappers that own the task.

**Dialects.** Three caller surfaces on one port, for every model
([[025-dialect-surfaces]]):

| Path | Dialect |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/responses` | OpenAI Responses |

Which one the engine speaks natively is a manifest fact, not shim
wiring: `engine_dialect` (default `openai-chat`). The matching surface
proxies through untouched; the other two translate through the shared
`latere.ai/x/pkg/llmdialect` package. What a translation cannot carry is
**reported, not dropped** — returned in the `X-LLMOps-Compat-Loss`
response header and counted in `llmops_dialect_loss_total{dialect,field}`,
so a lossy pairing is visible to the caller that hit it.

The **Lux dialect is deliberately not served here**: that surface belongs
to the gateway, which embeds the same package ([[009-lux-integration]]);
duplicating it per pod would re-implement gateway concerns.

## Components

1. **`llmops serve --manifest <path>`** (`internal/runtime`) — reads the
   manifest, resolves the weight store, prepares weights (below), renders
   engine CLI args, starts the engine and fronts it with the shim. One
   image per engine (`Dockerfile.sglang`, `Dockerfile.vllm`) for the
   container mode, engine versions pinned per
   [[001-inference-engine-selection]]; the same binary is installed
   directly on a host for the bare-metal mode.
2. **Weight preparation**, three modes selected in the manifest:
   - `load: nvme-cache` (default, both engines): sync S3 prefix →
     node-local NVMe path via `s5cmd` (verify against `_manifest.json`),
     then point the engine at the local dir. Idempotent: verified files
     are not re-fetched; concurrent pods on one node share the cache
     (flock).
   - `load: s3-stream` (vLLM only): `--load-format runai_streamer`
     directly from the S3 URI, concurrency 16–32. No disk staging.
   - `load: local` ([[021-local-weight-loading]]): the weights are
     already on the host's disk under a `--cache-root`, verified in place
     against `_manifest.json`. No bucket, and the manifest names no
     `s3_prefix` — which is what lets one manifest describe a model on
     any machine.
3. **Health surface** — engines expose their own health endpoints; a tiny
   sidecar-free shim maps them to the latere contract: `/healthz` (process
   up), `/ready` (weights verified **and** engine reports ready),
   `/metrics` (engine's Prometheus output passed through, plus
   `llmops_weights_load_seconds`, `llmops_dialect_loss_total` and
   `llmops_speculator_info`).

## Manifest schema (`models/<name>.yaml`)

```yaml
name: kimi-k2.7-code
hf_repo: moonshotai/Kimi-K2.7-Code
revision: <sha>                 # pinned; matches S3 prefix
s3_prefix: s3://latere-models/moonshotai/Kimi-K2.7-Code/<sha>/
format: int4-qat
license: modified-mit           # + license_note field for clauses
runtime: sglang                 # sglang | vllm | custom
# image: ghcr.io/latere-ai/...  # required iff runtime: custom
deploy: k8s                     # k8s (default) | bare-metal, specs/020
engine_dialect: openai-chat     # what the engine speaks, specs/025
load: nvme-cache                # nvme-cache | s3-stream | local
gpu: { type: h200, count: 8, nodes: 1 }
context_max: 262144
args:                           # engine-specific flags, verbatim
  - --tp-size=8
  - --tool-call-parser=kimi_k2
  - --reasoning-parser=kimi_k2
```

`system_prompt`, and the `speculators` map a fast path selects from
([[027-qwen-fast-path]]), are the two optional blocks on top of this.

Schema validated by a `llmops validate` subcommand (test-covered); CI
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
   `llmops_weights_load_seconds`.
3. NVMe cache mode: second pod start on a warm node skips download
   (test asserts no S3 GETs); corrupted cache file is detected and
   re-fetched (test).
4. `s3-stream` mode boots the same test model on vLLM with zero disk
   staging.
5. Manifest validation rejects unknown fields, unpinned revisions, and
   engine/args mismatches; `runtime: custom` without `image` is rejected
   (unit tests).
6. All three caller surfaces return well-formed responses (and SSE
   streams) for any `engine_dialect`: the matching one proxied untouched,
   the other two translated, with the loss report in
   `X-LLMOps-Compat-Loss` and `llmops_dialect_loss_total`. Malformed
   requests 400, engine failures pass through (tests against a fake
   engine). Detail in [[025-dialect-surfaces]].

## Non-goals

- Custom inference endpoints or prompt logic (engines' OpenAI API is the
  surface; Lux adds policy).
- Multi-node launch (leader/worker wiring lives in [[008-k8s-serving]]).
- Autoscaling, PD-disaggregation, KV-cache tiering.

## Verification

- CI: manifest validation + shim unit tests; single-GPU e2e with the test
  model (needs a GPU runner — if unavailable, e2e runs via `make e2e` on a
  GPU node and is a release gate, not a PR gate).
