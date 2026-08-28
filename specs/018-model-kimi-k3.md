---
title: "Model: Kimi-K3 (frontier multimodal; new B300 pool)"
status: draft
depends_on:
  - 004-model-kimi-k2.7-code.md
  - 008-k8s-serving.md
affects:
  - models/kimi-k3.yaml
  - deploy/kimi-k3/
  - internal/deploycheck/
  - Dockerfile.sglang
effort: large
created: 2026-08-02
updated: 2026-08-02
author: changkun
dispatched_task_id: null
---

# Model: Kimi-K3 (frontier multimodal; new B300 pool)

## Overview

The successor to [[004-model-kimi-k2.7-code]] and the largest thing this
repo will hold: 2.8T parameters, 104B active (16 of 896 experts), Kimi
Delta Attention + Gated MLA with Attention Residuals, native vision, 1M
context, MXFP4 weights with MXFP8 activations from quantization-aware
training. At **1561 GB** it more than doubles the frozen-weights
registry on its own ([[002-weights-registry]] budgeted ~2.7 TB for the
first four models).

Three things make this more than another model manifest, and all three
are gates rather than tasks:

1. **It needs hardware we do not have.** Every shape that fits on the
   h200 or b200 pools is multi-node.
2. **It needs an engine image we do not build.** K3 support ships in a
   dedicated CUDA 13 SGLang image; the shared image cannot serve it.
3. **It has a license with a Model-as-a-Service clause**, and Lux is a
   gateway. Same shape as [[006-model-minimax-m3]] AC0.

Mirror the weights regardless of all three — freezing weights we intend
to serve is the point of this repo, and upstream repos mutate.

## Facts (verified 2026-08-02)

- HF: `moonshotai/Kimi-K3`, revision
  `9f62e4e9fffbd0a83ddd60e1c209d828994b3569` (published 2026-07-27),
  **1561 GB**, 96 safetensors shards. 93 layers (69 KDA + 24 Gated MLA),
  896 experts / 16 active / 2 shared, MoonViT-V2 vision encoder (401M),
  160K vocab, context 1,048,576.
- Parsers: `kimi_k3` for both reasoning and tool calls. K3 **always**
  thinks; depth is `reasoning_effort` = low | high | max (default max).
- Preserved thinking history: multi-turn and tool-call turns must echo
  the complete assistant message back — `reasoning_content` and
  `tool_calls` included, not just `content`. A client that drops
  `reasoning_content` silently degrades the model.
- Sampling fixed by the model: `temperature=1.0`, `top_p=0.95`.
- Engines: SGLang via `lmsysorg/sglang:kimi-k3` (CUDA 13, needs an
  **r580+ NVIDIA driver**); vLLM 0.27.0 via `vllm/vllm-openai:kimi-k3`,
  also cu130-only. Both vendors label the K3 recipes **pre-release /
  "Final Verification In Progress"** as of today — the recipes run, but
  no serving round on final weights has landed. Every number below is a
  starting point to re-measure, not a pin.
- Published shapes: B300 1x8 · GB300 2x4 · B200 2x8 · GB200 4x4 ·
  H200 2x8 (4x8 high-throughput) · H100 4x8 · MI350X/MI355X 1x8.

## Hardware decision

**New `latere.ai/gpu-pool: b300` pool, one 8x B300 node, TP8 + DCP8.**

8x B300 is 2304 GB of HBM against 1561 GB of weights — ~740 GB left for
the KDA state pool, the MLA KV pool, and activations. It is the only
option that keeps K3 single-node on our own hardware, and the vendor
calls B300 1x8 the accuracy-first default. Cost: procurement, and a node
pool whose driver is r580+ from day one, which the existing h200/b200
pools are not.

Fallback if procurement slips: **H200 2x8, TP16/EP16** (Marlin +
FlashMLA, symm-mem, cross-node NIC pinned via `NCCL_SOCKET_IFNAME` /
`GLOO_SOCKET_IFNAME` / `SGLANG_HOST_IP`). That fallback is multi-node,
which is why AC3 below exists: today `deploycheck` never looks at
`workerTemplate`, so a 2-node deploy would pass CI with its worker
container completely unvalidated. The validator has to be able to see
the fallback before we are allowed to take it.

## License gate (AC0 — blocks Lux exposure)

Kimi K3 License, §2–§4:

- §2 **Model as a Service**: giving a third party access to inference in
  a way that lets them exercise meaningful control over inputs or
  parameters. If we operate such a business and aggregate revenue
  exceeds **$20M over any consecutive 12 months**, a separate agreement
  with Moonshot AI is required *before* any commercial use.
- §3 Attribution: display "Kimi K3" prominently above 100M MAU or $20M
  monthly revenue.
- §4(a) carves out **internal use** — use that does not make the model,
  its outputs, or its capabilities available to third parties. §2 and §3
  do not apply to it.

The question this gate answers is therefore not "how much revenue" but
**whether serving K3 through Lux counts as internal use**. Lux fronts
latere applications today; if a Lux virtual key is ever issued outside
latere, §2 engages. Record the conclusion (with the date and who made
it) in `models/kimi-k3.yaml`'s `license_note` before exposure, exactly
as [[006-model-minimax-m3]] does.

## Acceptance criteria

0. **License gate**: internal-use determination recorded in the manifest
   `license_note`. Blocks AC7.
1. Mirrored to S3 at the pinned revision, `mirror verify` clean. Needs
   ≥1.6 TB of scratch on the mirror Job and roughly doubles registry
   storage — size both before starting.
2. `models/kimi-k3.yaml` validates: `sglang`, b300 x8 x1, TP8 + DCP8,
   `kimi_k3` parsers, `--trust-remote-code` (K3 ships `auto_map` custom
   code and will not load without it).
3. `deploycheck` validates `workerTemplate` — image, GPU limit, and
   presence — whenever `gpu.nodes > 1`, with a test that fails without
   the rule. Closes the gap the H200 fallback would otherwise walk into.
4. `Dockerfile.sglang` builds `open-llms-runtime-sglang-k3` from the
   pinned CUDA 13 K3 image, selected with
   `--build-arg SGLANG_IMAGE=…`; the two images differ only in their
   base, so they share one Dockerfile. `make release` publishes it
   alongside the others. The shared `open-llms-runtime-sglang` image is **not** bumped
   to cu130 — that would force an r580+ driver on the h200/b200 pools
   for the sake of a model that does not run there.
5. Serves on the 8x B300 node via LWS. `--mamba-full-memory-ratio` is
   the one sizing flag and is workload-dependent (it divides the KDA
   state pool from the MLA KV pool); compute it from the vendor
   calculator against our measured average request length and record the
   value, do not guess it. Read back `max_total_num_tokens` and the
   admitted-request cap after boot.
6. e2e suite (`make e2e-kimi-k3`): chat, streaming, tool-call round-trip
   **with `reasoning_content` echoed back**, all three `reasoning_effort`
   levels, an image-input request (first multimodal request in the
   fleet), and a ≥256K-context request.
7. Registered in Lux as `open-llms/kimi-k3` — gated on AC0. Verify Lux
   passes `reasoning_effort` through and does not strip
   `reasoning_content` from assistant turns on the way back in.
8. Baseline benchmark recorded (010 harness), and re-run once the vendor
   marks the B300 recipe verified.

## Non-goals

- DCP/EP/PD-disaggregation tuning and the large-scale 16–64 GPU presets.
- Video input (the model card claims video; the served surface here is
  text + image only until an e2e test covers video).
- Retiring [[004-model-kimi-k2.7-code]] — K2.7-Code stays; it is a
  different price point on a pool K3 cannot use.
- Lens fitting for K3 ([[016-bigmodel-lens-fitting]] governs that, and
  its cost gate applies here more than to any other model).

## Verification

- AC3 is a unit-test gate in `make test`; AC2/AC4 are `make validate`
  plus the repo-consistency test and an image build.
- AC0 is a written record, checked before AC7 — no Lux route exists
  until it is in the manifest.
- AC6 e2e green on the B300 node is the release gate; AC5/AC8 numbers
  land in the Outcome section.
