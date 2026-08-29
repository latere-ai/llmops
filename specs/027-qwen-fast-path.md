---
title: "Model: Qwen3.8-27B fast path (NVFP4 + speculative decoding)"
status: draft
depends_on:
  - 019-gb10-serving-target.md
  - 021-local-weight-loading.md
  - 022-model-qwen3.8-27b.md
affects:
  - models/qwen3.8-27b-fast.yaml
  - deploy/qwen3.8-27b-fast/
  - internal/manifest/
  - Dockerfile.sglang
  - docs/practice.md
effort: medium
created: 2026-08-29
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Model: Qwen3.8-27B fast path

## The number that motivates this

[[022-model-qwen3.8-27b]] serves undamaged BF16 weights at a **measured
3.0 tok/s**. Claude Sonnet 5 measures around 95 tok/s on Anthropic's API.
Thirty times slower is not a tuning complaint; it is the difference
between a model you work in and one you demonstrate.

The same model on the same GB10 class has been measured at **51.5 tok/s**
by [MiaAI-Lab](https://github.com/MiaAI-Lab/Qwen3.8-27B-SGLang-DGX-Spark),
which publishes its configuration and its numbers:

| Probe | DSpark | MTP/EAGLE | DFlash2 |
|---|---|---|---|
| Code | **51.5** | 34.5 | 50.9 |
| Long essay | 18.3 | 24.1 | 25.4 |

The 2.8x spread between code and prose is the useful part of that table:
memory bandwidth is indifferent to what is being generated, so a gap that
large is draft-token acceptance, not throughput.

## Where the 17x comes from, and why it is not a misconfiguration

Two independent multipliers, neither of which 022 uses:

```
BF16, one token per forward pass   54.0 GB/token   →   3.0 tok/s  measured
NVFP4 W4A4                         13.5 GB/token   →  ~12 tok/s   4x fewer bytes
+ speculative decoding                             →  18-51 tok/s draft acceptance
```

The 4-bit step lands on the ~12 tok/s already predicted in
`docs/practice.md`, so nothing here contradicts the bandwidth model.

**It does, however, break the formula that document states.**
`tok/s ≈ efficiency × bandwidth / bytes_per_token` assumes one token per
forward pass. Speculative decoding reads the weights once and verifies
several tokens against that read, so the honest form multiplies by
accepted tokens per step. That correction belongs in `docs/practice.md`
regardless of whether this spec is built.

## What this costs, stated plainly

022's argument for this model over the compressed DeepSeek
([[023-model-deepseek-v4-flash-0731-gb10]]) was that undamaged weights
beat a large model squeezed to fit. **This spec spends exactly that.**

NVFP4 is 4-bit weights *and* 4-bit activations. It is a real quality
reduction, and the honest framing is that we now have a measured price
for the principle: 17x. That is high enough that the answer may change,
which is why this is a separate endpoint rather than an edit to 022.

**Both endpoints stay.** The same rule as
[[023-model-deepseek-v4-flash-0731-gb10]]: a caller must be able to tell
which one answered, so the precision is in the name.

```
qwen3.8-27b        BF16, undamaged, ~3 tok/s      (022)
qwen3.8-27b-fast   NVFP4 + speculation, ~50 tok/s (this spec)
```

One GPU means one at a time ([[019-gb10-serving-target]]), so this is a
choice made when starting the box, not a routing decision.

## The artifacts exist and are pinnable

Verified 2026-08-29. This is the reason to build now rather than wait:

| Artifact | Repo | Size | License |
|---|---|---|---|
| NVFP4 weights | `RadixArk/Qwen3.8-27B-NVFP4-BF16-LMHead` | 23.8 GB | apache-2.0 |
| DSpark draft head | `RadixArk/Qwen3.8-27B-DSpark` | 3.7 GB | **other** |
| DFlash2 draft head | `z-lab/Qwen3.8-27B-DFlash2` | 3.8 GB | apache-2.0 |
| FP8 (fallback) | `Qwen/Qwen3.8-27B-FP8` | 30.9 GB | apache-2.0 |

**None of these are Qwen's own**, except the FP8. `RadixArk` and `z-lab`
are third parties, so [[021-local-weight-loading]]'s freeze rules carry
the weight they were written for: pin the revision, checksum every file,
and record in `license_note` that these are third-party requantizations
of an Apache-2.0 base. The DSpark head's `other` license needs reading
before it ships.

## Three decisions this forces

### 1. The engine becomes SGLang

All three speculative paths are SGLang's (`--speculative-algorithm
dspark|eagle|dflash`). 022 chose vLLM because the pinned SGLang v0.5.16
predates this model — a decision made with no throughput number attached.
There is one now.

Worse for us, the reference configuration pins
`lmsysorg/sglang:qwen38-27b`: a **model-specific image tag**, not a
release. DFlash2 needs a further derived image. So this is not an engine
version bump, it is a per-model engine build.

### 2. That collides with the bare-metal deploy mode

[[020-bare-metal-packaging]] installs a pinned engine into a virtualenv;
this reference runs a container. Either we find the pip-installable
equivalent of that image tag, or this model runs `deploy: k8s` on a box
with no cluster — which is the shape 019 rejected.

**Resolve this before building.** It is the one open question here, and
it is a real fork: a per-model container on a lab box is a different
operational story from a pinned venv.

### 3. The 0.80 memory ceiling looks too conservative

[[019-gb10-serving-target]] AC2 caps the engine's share of the unified
pool at 0.80, and AC3a records that the figure is *derived, not
measured*. The reference configuration runs `--mem-fraction-static 0.90`
and `0.95` on this hardware, apparently without taking the host down.

That is external evidence against our cap. It should be measured rather
than copied — but a ceiling we set from arithmetic, contradicted by
someone running the same box, is exactly what AC3a existed to catch.

## Configuration to start from

From the reference, with `--kv-cache-dtype fp8_e4m3` halving the KV cache
that [[022-model-qwen3.8-27b]] measured at 18.23 GiB:

```
--speculative-algorithm dspark
--speculative-dspark-block-size 7
--speculative-num-draft-tokens 8
--kv-cache-dtype fp8_e4m3
--chunked-prefill-size 8192
--mem-fraction-static 0.90
```

DSpark is the starting point because it is fastest on code, which is what
this endpoint is for. DFlash2 is within noise of it on code and clearly
better on prose, so it is the one to compare against, not an afterthought.

## Acceptance criteria

- **AC1** The NVFP4 weights and the chosen draft head are pulled, frozen
  and verified by `llmops freeze`, with `license_note` recording the
  third-party requantization and the DSpark head's license reviewed.
- **AC2** `models/qwen3.8-27b-fast.yaml` validates and serves under the
  name `qwen3.8-27b-fast`. A test asserts it is never aliased to
  `qwen3.8-27b`, for the reason in
  [[023-model-deepseek-v4-flash-0731-gb10]].
- **AC3** Measured throughput on our box, by `llmops bench`, on both a
  code prompt and a prose prompt — the two are different numbers and
  reporting one is misleading.
- **AC4** A quality comparison against the BF16 endpoint on the work this
  is for. A 17x speedup at an unmeasured quality cost is not a decision,
  it is a hope.
- **AC5** The memory fraction actually used is measured, and
  [[019-gb10-serving-target]] AC2's 0.80 ceiling is either confirmed or
  raised with the measurement behind it.
- **AC6** The deploy-mode question in decision 2 is answered in this spec
  before the model ships.
- **AC7** `docs/practice.md`'s throughput formula is corrected to account
  for speculative decoding. This lands regardless of the rest.

## Out of scope

- Retiring the BF16 endpoint. It is the quality reference this one is
  measured against.
- Producing our own quantization. Published NVFP4 exists; a 27B
  calibration run is its own project.
- Speculative decoding for other models. DeepSeek's case is
  [[023-model-deepseek-v4-flash-0731-gb10]], where the draft head does
  not fit the budget.
