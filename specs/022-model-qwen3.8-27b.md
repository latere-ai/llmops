---
title: "Model: Qwen3.8-27B (dense multimodal, GB10)"
status: draft
depends_on:
  - 019-gb10-serving-target.md
  - 020-bare-metal-packaging.md
  - 021-local-weight-loading.md
affects:
  - models/qwen3.8-27b.yaml
  - deploy/qwen3.8-27b/
  - README.md
effort: medium
created: 2026-08-28
updated: 2026-08-28
author: changkun
dispatched_task_id: null
---

# Model: Qwen3.8-27B (dense multimodal, GB10)

## Overview

Every model in the registry so far is a large sparse MoE on a multi-GPU
node. This one is none of those things: 27B **dense**, natively
**multimodal**, and small enough to serve at **full BF16 precision with
no quantization** on a single [[019-gb10-serving-target]] node, from the
vendor's own checkpoint.

That is the reason to serve it. On this hardware class the alternative
([[023-model-deepseek-v4-flash-0731-gb10]]) is a much larger model
compressed below the precision it was trained for. This one is
undamaged and leaves roughly 45 GB of the node free. Those are different
products.

It is also the first model that makes the registry's scope explicit:
**open-llms serves what we choose to own, not only frontier-scale MoE.**
If that is wrong, this spec is where to say so.

## Facts (verified 2026-08-28)

- HF `Qwen/Qwen3.8-27B`, revision
  `1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0`, published 2026-08-14.
  **Apache-2.0** — no gate, no acceptance step, the least encumbered
  license in the registry.
- 27B dense, released as **BF16**. Native vision and video
  understanding in the same checkpoint.
- **262,144 native context**, extensible to ~1M via YaRN.
- Hybrid attention, 64 layers in a 3:1 pattern: **48 Gated DeltaNet**
  linear-attention layers and **16 Gated Attention** full-attention
  layers. Full-attention layers carry 24 Q heads and **4 KV heads at 256
  head dim**; the linear layers hold a fixed-size recurrent state that
  does not grow with sequence length.

## Engine

**vLLM v0.28.0**, installed on the host as a pinned virtualenv per
[[020-bare-metal-packaging]] — there is no container in this deploy
mode. The same version is pinned in `Dockerfile.vllm` for the fleet, so
the two modes do not drift apart on engine version.

The checkpoint's `config.json` declares
`architectures: ["Qwen3_5ForConditionalGeneration"]` with
`model_type: qwen3_5` — note the config uses the 3.5 family name, not
the 3.8 marketing name, which is the string any engine matches on.
vLLM v0.28.0's model registry names that exact architecture in its
**multimodal** table, backed by a real `qwen3_5.py` module. So vision is
supported, not merely the text path (`Qwen3_5ForCausalLM` is a separate
entry).

SGLang is the fleet primary ([[001-inference-engine-selection]]) and the
default for a new model. It is not the choice here because the pinned
SGLang tag `v0.5.16` predates this model, so its coverage of these
layers is unverified, and bumping SGLang for one model on one node is
more disruption than using the second engine. If a later SGLang bump
carries the architecture, moving this model is a manifest edit.

If SGLang is bumped later for another reason and carries the
architecture, moving this model is a manifest edit and nothing else.

## Memory budget

The KV cache is the interesting term, and it is small because only 16 of
64 layers cache anything:

```
kv_per_token = 16 layers x 4 kv_heads x 256 dim x 2 (K,V) x 2 bytes
             = 65,536 bytes = 64 KiB / token
```

```
weights   27B x 2 bytes (BF16)          =  54.0 GB
kv_cache  262,144 tokens x 64 KiB       =  17.2 GB
                                           ------
                                            71.2 GB   of ~115 GB usable
```

A conventional 27B caching on every layer would want roughly four times
the KV and would not reach native context on this node. The 3:1 hybrid
is what makes 262K affordable, so **`--max-model-len 262144` is the
configuration this spec claims**, not a ceiling to tune down.

### Choosing the memory fraction

vLLM does not treat `--gpu-memory-utilization` as a ceiling it stays
under: it profiles the model and then **allocates KV blocks to fill the
budget**. So the fraction sets actual consumption, not a cap, and
[[019-gb10-serving-target]] AC2's 0.80 bound would hand this model
~102 GB and fill the node — leaving the host the same ~26 GB it would
have had with a far larger model.

This model is sized deliberately below the bound:

```
0.65 x 128 GB                        =  83.2 GB   engine budget
  weights                            =  54.0 GB
  activations + CUDA graphs (est.)   =   6.0 GB
                                        ------
  available for KV                   =  23.2 GB   (17.2 GB needed)
node left to the host and page cache =  44.8 GB
```

0.65 covers the 262K KV requirement with margin and leaves roughly 45 GB
of the node genuinely free. Raising it to 0.80 buys concurrency headroom
at the cost of that margin; it is a deliberate change, not a default.

## Weights

The vendor checkpoint is served **as published** — BF16 safetensors, no
conversion, no derived artifacts, no third-party requantization. `mirror
pull` fetches `Qwen/Qwen3.8-27B` at the pinned SHA, `mirror freeze`
writes `_manifest.json`, and [[021-local-weight-loading]] verifies it in
place at launch. The freeze chain has exactly one link and it ends at
the vendor.

This is the simplest weight story in the registry, and it is the second
reason to prefer this model on this node.

## Manifest sketch

```yaml
name: qwen3.8-27b
hf_repo: Qwen/Qwen3.8-27B
revision: 1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0
format: bf16
license: apache-2.0
runtime: vllm
deploy: bare-metal
load: local
local_path: /var/lib/openllms/Qwen/Qwen3.8-27B/1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0/
gpu: { type: gb10, count: 1, nodes: 1 }
context_max: 262144
args:
  - --max-model-len=262144
  - --gpu-memory-utilization=0.65
```

This is the first manifest in the registry with `deploy: bare-metal`, so
it is also the first exercise of [[020-bare-metal-packaging]]'s unit
file and install path.

## Acceptance criteria

- **AC1** `models/qwen3.8-27b.yaml` validates, and `deploycheck` passes
  against `deploy/qwen3.8-27b/qwen3.8-27b.service` — the bare-metal arm
  of the check, with no LWS manifest present for this model.
- **AC2** The endpoint answers OpenAI `/v1/chat/completions` and
  `/anthropic/v1/messages` under the manifest name `qwen3.8-27b`.
- **AC3** **Vision works.** An image request returns a grounded answer
  through both the OpenAI and Anthropic surfaces. The vision tower is
  part of the checkpoint, so there is no separate artifact to forget —
  but there is also nothing that fails loudly if the engine silently
  serves text-only, which is why this is tested against a real image
  rather than inferred from a clean start.
- **AC4** A 262K-token request succeeds. The KV arithmetic above is the
  claim; this is the test of it.
- **AC5** Measured resident memory at full context sits within the
  83.2 GB engine budget, with at least 17.2 GB actually allocated to KV
  so the 262K claim in AC4 is real and not a short cache. If either
  number is wrong, the arithmetic above is corrected here rather than
  the fraction being raised silently.
- **AC6** No tokens/sec target is set. There is no published figure for
  this model on this GPU, so [[010-observability-bench]]'s harness
  establishes the baseline rather than checking one.
- **AC7** README's target-model table carries the model, its class
  (dense, multimodal) and its GPU class.

## Out of scope

- YaRN extension beyond 262K native.
- Quantized variants. The point of this model on this node is that it
  needs none.
- Video input. The checkpoint supports it; the serving surface for it is
  not specified here.
