---
title: Frozen Weights Registry (HF → S3 mirror)
status: draft
depends_on:
  - 000-architecture.md
affects:
  - tools/mirror/
  - models/
effort: medium
created: 2026-07-18
updated: 2026-07-18
author: changkun
dispatched_task_id: null
---

# Frozen Weights Registry (HF → S3 mirror)

## Overview

Own the weights. Every model we serve is mirrored **once** from Hugging Face
into a versioned, checksummed, immutable S3 prefix, and all serving/loading
happens from that prefix — never from HF again. This removes upstream
mutation/deletion risk, HF rate limits, and repeated multi-hundred-GB
downloads, and it is the prerequisite for future post-training on weights we
control.

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

## Mirror tool (`tools/mirror`)

CLI (Go or Python — decide in impl, prefer Go for parity with sibling repos):

1. `mirror pull <hf_repo>[@revision]` — resolve revision to SHA; download via
   `hf download` with `hf_transfer` enabled; **safetensors-only policy**
   (reject pickle/bin); verify SHA256 against HF LFS OIDs.
2. `mirror push` — upload with `s5cmd` to the revision-pinned prefix; write
   `_manifest.json` last (its presence = mirror complete/atomic).
3. `mirror verify <s3_prefix>` — re-hash or use S3 Checksum-SHA256 to confirm
   integrity against the manifest.
4. `mirror ls` — list mirrored models/revisions.

Runs as a k8s Job (needs bandwidth + scratch disk), also runnable locally.

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

BF16/base checkpoints (MiniMax-M3 854 GB, GLM-5.2 1.51 TB,
DeepSeek-V4-Pro-Base 1.61 TB) are **deferred** until post-training becomes
concrete — they add ~4 TB and serve no inference purpose.

## Acceptance criteria

1. `mirror pull && mirror push` on a small test repo (<5 GB) produces a
   revision-pinned prefix with a valid `_manifest.json`; re-running is
   idempotent (no re-upload of verified files).
2. `mirror verify` detects a corrupted/truncated file (e2e test with injected
   corruption).
3. Non-safetensors weight files are rejected with a clear error (test).
4. All four serving-format repos above mirrored; sizes and SHA256 manifests
   match HF; documented in `models/*.yaml`.
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

- Unit + e2e tests in `tools/mirror` (corruption, idempotency, policy).
- One full-size mirror timed and documented (expected: hours at multi-Gbps;
  one-time cost per model revision).
