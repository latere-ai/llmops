---
title: Frozen Weights Registry (HF → S3 mirror)
status: partial
depends_on:
  - 000-architecture.md
affects:
  - internal/mirror/
  - cmd/llmops/
  - models/
effort: medium
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Frozen Weights Registry (HF → S3 mirror)

## Overview

Own the weights. Every model we serve is mirrored **once** from Hugging Face
into a versioned, checksummed, immutable store, and all serving/loading
happens from that store — never from HF again. This removes upstream
mutation/deletion risk, HF rate limits, and repeated multi-hundred-GB
downloads, and it is the prerequisite for future post-training on weights we
control.

The store is an S3 prefix for the fleet, and a host's own directory for a
machine with no bucket in the picture ([[021-local-weight-loading]]). What
freezes the weights is the pinned revision plus the per-file checksums,
not the storage behind them: every primitive below is store-agnostic and
the layout is identical in both.

## S3 layout

```
s3://latere-models/<hf_org>/<hf_name>/<hf_revision_sha>/
  ├── <original HF file tree, verbatim>          # safetensors, configs, tokenizer
  └── _manifest.json                             # written last, marks mirror complete
```

- The **HF commit SHA** in the prefix makes each mirror immutable-by-convention
  and content-addressed-by-convention; loaders (vLLM Run:ai streamer) accept it
  as a plain S3 URI.
- Bucket: versioning ON; S3 Object Lock (governance mode) on the prefix once
  `_manifest.json` lands. Lifecycle: no expiry; weights are frozen capital.
- `_manifest.json`: HF repo id, revision SHA, mirror timestamp, total bytes,
  per-file `{path, size, sha256}`. HF LFS OIDs are SHA256 — verify at mirror
  time without re-hashing the source.

## Mirror subcommands (`internal/mirror`, driven from `cmd/llmops`)

Go, no cgo. Five subcommands of the one `llmops` binary
([[024-single-cli]]) rather than a separate tool:

1. `llmops pull <hf_repo>[@revision]` — resolve revision to SHA; download via
   `hf download` with `hf_transfer` enabled; **safetensors-only policy**
   (reject pickle/bin), failing closed on an unknown extension; verify
   SHA256 against HF LFS OIDs.
2. `llmops freeze <hf_repo>@<sha> --dir <dir>` — write `_manifest.json` into
   a directory that already holds the files, so a host that serves from its
   own disk gets the same freeze guarantee with no upload
   ([[021-local-weight-loading]]).
3. `llmops push` — upload with `s5cmd` to the revision-pinned prefix; write
   `_manifest.json` last (its presence = mirror complete/atomic).
4. `llmops verify <prefix>` — re-hash every file against the manifest.
   Takes an `s3://` URI or a plain path; the store type is resolved from
   the argument.
5. `llmops list` — list mirrored models/revisions.

Runs as a k8s Job (needs bandwidth + scratch disk), also runnable
locally. `deploy/mirror/job.yaml` is the parameterized Job
(`Dockerfile.mirror` image); it blocks on two infra decisions recorded
there: the dedicated bucket (S3 vs DO Spaces via `S3_ENDPOINT_URL`) and
the `mirror-s3` credentials Secret.

## Model manifests (`models/`)

One `models/<name>.yaml` per served model: HF repo, pinned revision SHA, S3
prefix, weight format (e.g. FP8/INT4/MXFP8), license note, and the serving
config reference. This file is the single source of truth a deploy consumes.

## Initial mirror set (~2.7 TB, serving formats only)

| HF repo | Format | Size |
|---|---|---|
| MiniMaxAI/MiniMax-M3-MXFP8 | MXFP8 | 444 GB |
| zai-org/GLM-5.2-FP8 | FP8 | 756 GB |
| moonshotai/Kimi-K2.7-Code | INT4 QAT (native) | 595 GB |
| deepseek-ai/DeepSeek-V4-Pro | FP4+FP8 | 865 GB |

The 2026-08 refresh adds **1.7 TB** on top, taking the registry past
4.4 TB — K3 alone outweighs the original four:

| HF repo | Format | Size | Spec |
|---|---|---|---|
| deepseek-ai/DeepSeek-V4-Flash-0731 | FP4+FP8 | 167 GB | [[017-model-deepseek-v4-flash-0731]] |
| moonshotai/Kimi-K3 | MXFP4/MXFP8 | 1561 GB | [[018-model-kimi-k3]] |

Kimi-K3 is also the first repo that needs more scratch than any single
model before it — size the mirror Job's volume at ≥1.6 TB for it.

BF16/base checkpoints (MiniMax-M3 854 GB, GLM-5.2 1.51 TB,
DeepSeek-V4-Pro-Base 1.61 TB) are **deferred** until post-training becomes
concrete — they add ~4 TB and serve no inference purpose.

## Acceptance criteria

1. `llmops pull && llmops push` on a small test repo (<5 GB) produces a
   revision-pinned prefix with a valid `_manifest.json`; re-running is
   idempotent (no re-upload of verified files).
2. `llmops verify` detects a corrupted/truncated file (e2e test with injected
   corruption).
3. Non-safetensors weight files are rejected with a clear error (test).
4. All six S3-served repos above mirrored; sizes and SHA256 manifests
   match HF; documented in `models/*.yaml`. (Qwen3.8-27B is the seventh
   manifest and is deliberately not here: `load: local` names no
   `s3_prefix`, and its weights are frozen in place on the host.)
5. vLLM loads a model directly from the mirrored S3 prefix (covered in
   [[003-serving-runtime]] e2e).

## Non-goals

- OCI packaging of weight bytes (registries break down at 500 GB+; OCI is
  considered later only as a provenance/metadata layer — see Future).
- P2P distribution (Dragonfly) — only when node count makes S3 egress the
  bottleneck.
- BF16/base checkpoints for post-training.

## Future

- Sigstore/OMS model signing at mirror time (OpenSSF Model Signing spec).
- Dragonfly P2P layer for many-node fan-out.

## Verification

- Unit + e2e tests in `internal/mirror` and `cmd/llmops` (corruption,
  idempotency, policy), plus the full-stack local run in
  [[011-local-e2e]] against MinIO.
- One full-size mirror timed and documented (expected: hours at multi-Gbps;
  one-time cost per model revision).
