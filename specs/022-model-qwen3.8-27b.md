---
title: "Model: Qwen3.8-27B (dense multimodal, GB10)"
status: draft
depends_on:
  - 019-gb10-serving-target.md
  - 020-llamacpp-runtime.md
  - 021-local-weight-loading.md
affects:
  - models/qwen3.8-27b.yaml
  - deploy/qwen3.8-27b/
  - cmd/mirror/
  - README.md
effort: medium
created: 2026-08-28
updated: 2026-08-28
author: changkun
dispatched_task_id: null
---

# Model: Qwen3.8-27B (dense multimodal, GB10)

## Overview

Every model in the registry so far is a large sparse MoE served on a
multi-GPU node. This one is none of those things: 27B **dense**, natively
**multimodal**, and small enough to run at **full BF16 precision with no
quantization at all** on a single [[019-gb10-serving-target]] node.

That is the reason to serve it. On this hardware class the alternative
([[023-model-deepseek-v4-flash-0731-gb10]]) is a very large model
compressed to roughly two bits. This one is an undamaged model that
leaves 40 GB of the node idle. Those are different products, and the
registry should carry both rather than pretend one dominates.

It is also the first model that makes the registry's scope explicit:
**open-llms serves what we choose to own, not only frontier-scale MoE.**
If that is wrong, this spec is where to say so.

## Facts (verified 2026-08-28)

- HF `Qwen/Qwen3.8-27B`, revision
  `1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0`, published 2026-08-14.
  **Apache-2.0** — no gate, no acceptance step, the least encumbered
  license in the registry.
- 27B dense. Released as **BF16**.
- Native vision and video understanding; the projector is a separate
  artifact, not fused into the language weights.
- **262,144 native context**, extensible to ~1M via YaRN.
- Hybrid attention, 64 layers in a 3:1 pattern:
  **48 Gated DeltaNet** linear-attention layers and **16 Gated
  Attention** full-attention layers. Full-attention layers carry 24 Q
  heads and **4 KV heads at 256 head dim**; the linear layers hold a
  fixed-size recurrent state that does not grow with sequence length.
- Engine support: llama.cpp carries the Gated DeltaNet operators (landed
  May 2026, needs a current build, not a distro package); vLLM and SGLang
  also carry the architecture — but not on this GPU, per
  [[019-gb10-serving-target]].

## Memory budget

The KV cache is the interesting term, and it is small for a reason worth
writing down: only 16 of 64 layers cache anything.

```
kv_per_token = 16 layers x 4 kv_heads x 256 dim x 2 (K,V) x 2 bytes
             = 65,536 bytes = 64 KiB / token
```

```
weights   27B x 2 bytes (BF16)          =  54.0 GB
kv_cache  262,144 tokens x 64 KiB       =  17.2 GB
mmproj    vision projector (f16)        =   0.9 GB
                                           ------
                                            72.1 GB   of ~115 GB usable
```

A conventional 27B with full attention on every layer would want roughly
four times the KV cache and would not reach native context on this node.
The 3:1 hybrid is what makes 262K affordable, so **`-c 262144` is the
configuration this spec is claiming**, not a maximum to be tuned down.

## Weight format decision

llama.cpp needs GGUF. There are two ways to get one, and they have
different provenance.

**Chosen: convert from the vendor checkpoint ourselves.**
`mirror pull` fetches `Qwen/Qwen3.8-27B` at the pinned SHA, and a
conversion step produces GGUF locally. The freeze chain stays rooted at
the vendor revision, and the derived artifact records the tool version
that produced it.

**Rejected: pin a third-party GGUF repo.** It would be less work and it
is what [[023-model-deepseek-v4-flash-0731-gb10]] is forced into, but it
substitutes someone else's conversion for the vendor's weights while the
manifest still says Qwen. Where converting ourselves is affordable — and
at 27B F16 it plainly is — the provenance is worth the step.

The local store therefore holds two things under one pinned revision:

```
<local_path>/
  source/           vendor safetensors, SHA-256 per file from HF
  gguf/             derived: *.gguf + mmproj, our SHA-256
  _manifest.json    covers both, written last
```

Build order matters, because the derived files do not exist when the
vendor pull finishes: **`mirror pull` → convert → `mirror freeze`**. The
freeze runs last and writes the single `_manifest.json` covering both
trees. That write belongs to the mirror tool and happens before the
endpoint starts, so it does not conflict with
[[021-local-weight-loading]] AC4, which constrains the serving path only.
Re-running the conversion means re-running the freeze.

`weights_file` and `mmproj_file` from [[020-llamacpp-runtime]] point
into `gguf/`. Provenance for the derived files is the vendor revision
plus the recorded conversion command and llama.cpp SHA — that pair has
to be reproducible, which AC4 pins.

## Manifest sketch

```yaml
name: qwen3.8-27b
hf_repo: Qwen/Qwen3.8-27B
revision: 1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0
format: bf16-gguf
license: apache-2.0
runtime: llamacpp
load: local
local_path: /var/lib/openllms/Qwen/Qwen3.8-27B/1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0/
weights_file: gguf/Qwen3.8-27B-BF16-00001-of-00002.gguf
mmproj_file: gguf/mmproj-Qwen3.8-27B-f16.gguf
gpu: { type: gb10, count: 1, nodes: 1 }
context_max: 262144
args:
  - --n-gpu-layers=99
  - --ctx-size=262144
```

No `--mem-fraction-static` or `--gpu-memory-utilization`: on unified
memory those are the node-killer that [[019-gb10-serving-target]] AC3
rejects at validation time.

## Acceptance criteria

- **AC1** `models/qwen3.8-27b.yaml` validates, and `deploycheck` passes
  against `deploy/qwen3.8-27b/lws.yaml` on the gb10 pool with
  `nvidia.com/gpu: "1"`.
- **AC2** The conversion is reproducible: running it twice from the same
  vendor revision yields byte-identical GGUF, and `_manifest.json`
  records the llama.cpp SHA and the exact conversion command.
- **AC3** The endpoint answers OpenAI `/v1/chat/completions` and
  `/anthropic/v1/messages` under the manifest name `qwen3.8-27b`.
- **AC4** **Vision actually works, and cannot be dropped silently.**
  A manifest for `Qwen/Qwen3.8-27B` without `mmproj_file` fails
  validation — enforced the way `requiredArgs` already keys required
  flags on `hf_repo`, since validation cannot otherwise know a
  checkpoint has a vision tower. On top of that, an image request
  against the live endpoint returns a grounded answer. The schema rule
  catches the omission in CI; the request proves the projector is
  actually wired. Without the first, a missing field yields a
  text-only engine that starts cleanly and looks healthy, which is the
  failure mode this model is most likely to ship with.
- **AC5** Measured resident memory at full 262K context is within the
  72.1 GB budget above, or the budget is corrected in this spec. No
  tokens/sec target is set here — there is no published figure for this
  model on this GPU, so [[010-observability-bench]]'s harness
  establishes the baseline rather than checking one.
- **AC6** A 262K-token request succeeds. The KV arithmetic above is the
  claim; this is the test of it.
- **AC7** README's target-model table carries the model, its class
  (dense, multimodal) and its GPU class.

## Out of scope

- YaRN extension beyond 262K native.
- Quantized variants. The point of this model on this node is that it
  does not need one.
- Video input. The checkpoint supports it; the serving surface for it is
  not specified here.
