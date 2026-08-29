---
title: Kubernetes GPU Serving (LWS, NVMe cache, scheduling)
status: draft
depends_on:
  - 003-serving-runtime.md
affects:
  - deploy/
  - internal/deploycheck/
effort: medium
created: 2026-07-18
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Kubernetes GPU Serving (LWS, NVMe cache, scheduling)

## Overview

Deploy runtime images on bare-metal GPU nodes. Baseline (2026 modal
answer): **engine + LeaderWorkerSet (kubernetes-sigs/lws)**, even for
single-node models — LWS with size=1 groups costs nothing and makes the
DeepSeek-V4-Pro multi-node case (2x8 H100) a manifest change, not an
architecture change.

## Scope

1. **`deploy/<model>/lws.yaml`**: one LeaderWorkerSet per k8s model, GPU
   resource requests, node selectors for GPU type (h200/b200/b300 pools),
   PodMonitor for `/metrics`, PDB. Plain manifests, not kustomize
   overlays — see AC1 for why that changed.

   `deploy/` is no longer k8s-only: a `deploy: bare-metal` model owns a
   systemd unit in the same layout instead
   ([[020-bare-metal-packaging]]), and `internal/deploycheck` dispatches
   on the mode so neither is validated against the other's artifact. A
   model owns exactly one artifact; two would be two sources of truth for
   how it starts.
2. **NVMe cache prefetch DaemonSet** (per GPU pool): pre-syncs configured
   model prefixes from S3 to hostPath NVMe so pod cold-start skips the
   S3 pull; runtime's `nvme-cache` mode finds a warm cache. Cache eviction
   is manual (models are few and huge).
3. **Node prerequisites documented** (owned by infrastructure
   provisioning, consumed here): NVIDIA GPU Operator; an **r580+ NVIDIA
   driver on the b300 pool**, which the h200/b200 pools do not need and
   which is why [[018-model-kimi-k3]] cannot share their image; for
   multi-node later — Network Operator,
   RoCEv2 (not InfiniBand: 400GbE RoCEv2 is the 2026 inference default),
   NCCL env (`NCCL_SOCKET_IFNAME`, `NCCL_IB_HCA`, `NCCL_NET_GDR_LEVEL`).
4. **Gang scheduling**: none needed for size=1 groups; when multi-node
   arrives, adopt KAI Scheduler or Kueue — recorded as the trigger, not
   built now.

## GPU footprints (from model research, 2026-07-18)

| Model | Format | Min shape |
|---|---|---|
| Kimi-K2.7-Code | INT4 native | 1x node 8x H200, TP8 |
| GLM-5.2 | FP8 | 1x node 8x H200, TP8 + EP; 1M ctx needs fp8_e5m2 KV |
| MiniMax-M3 | MXFP8 | 1x node 8x H200 (MXFP8 from TP4) |
| DeepSeek-V4-Pro | FP4+FP8 | 8x B200/B300 (native FP4) or 8x H200 tight or 2x8 H100 |
| DeepSeek-V4-Flash-0731 | FP4+FP8 | 1x node 8x B200, TP8 ([[017-model-deepseek-v4-flash-0731]]) |
| Kimi-K3 | MXFP4/MXFP8 | 1x node 8x B300, r580+ driver ([[018-model-kimi-k3]]) |

Six models, three pools — six of the repo's eight manifests. The other
two are the GB10 pair, `qwen3.8-27b` and `qwen3.8-27b-fast`: one GB10
GPU has nothing for a scheduler to schedule, so they take the bare-metal
mode ([[019-gb10-serving-target]]).

## Acceptance criteria

1. Every k8s model's `deploy/<model>/lws.yaml` is consistent with its
   manifest — image, GPU count and type, node selector, mounted manifest,
   probes — checked by `llmops validate` in CI and by
   `internal/deploycheck` in tests. **Holds today for all six.**

   This replaces the original "`kustomize build` renders + golden files".
   Kustomize was never adopted: with one LWS per model and no overlays to
   compose, rendering has nothing to render, and a golden file proves the
   YAML did not change rather than that it agrees with the manifest.
   Cross-validation against `models/*.yaml` is the check that would have
   caught a real mistake, so it is the one that exists.
2. One real model (Kimi-K2.7-Code, per roadmap order) reaches `/ready` on
   a GPU node via LWS, loading from NVMe cache; kill the pod → recovers
   without re-downloading (warm cache).
3. Prefetch DaemonSet: node bootstrap → configured models present on NVMe
   with manifest verification; Prometheus gauge for cache state.
4. PodMonitor scrapes engine metrics into the cluster Prometheus.

## Non-goals

- GPU procurement / node provisioning.
- Multi-node PD-disaggregation, llm-d/Dynamo adoption (Future: triggered
  when a model needs >1 node in production — DeepSeek-V4-Pro on Hopper —
  or when utilization demands disagg).
- Autoscaling (models this size scale by capacity planning, not HPA).

## Verification

- CI golden-file rendering tests; e2e criterion 2 on real hardware as the
  release gate.
