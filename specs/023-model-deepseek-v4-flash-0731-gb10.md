---
title: "Model: DeepSeek-V4-Flash-0731 on GB10 (reduced-precision tier)"
status: draft
depends_on:
  - 017-model-deepseek-v4-flash-0731.md
  - 019-gb10-serving-target.md
  - 020-llamacpp-runtime.md
  - 021-local-weight-loading.md
affects:
  - models/deepseek-v4-flash-0731-q2.yaml
  - deploy/deepseek-v4-flash-0731-q2/
  - internal/manifest/
  - README.md
effort: medium
created: 2026-08-28
updated: 2026-08-28
author: changkun
dispatched_task_id: null
---

# Model: DeepSeek-V4-Flash-0731 on GB10 (reduced-precision tier)

## The problem this spec exists to prevent

[[017-model-deepseek-v4-flash-0731]] serves this checkpoint at FP4
experts + FP8 attention on 8x B200 as `deepseek-v4-flash-0731`. On a
single [[019-gb10-serving-target]] node the same checkpoint only fits at
**two to three bits per weight**. Same model card, same vendor revision,
materially different model.

The README's stated reason for owning this layer instead of renting a
router includes removing "silent model swaps". Serving both of these
under one model id would be exactly that swap, performed by us.

**Decision: this is a separate, separately-named endpoint. The two are
never routed under one Lux model id, and the name states the tier.**

```
deepseek-v4-flash-0731       FP4/FP8, 8x B200   (017)
deepseek-v4-flash-0731-q2    ~2-3 bit, 1x GB10  (this spec)
```

Named for the quantization, not the hardware: the GPU is an
implementation detail a caller does not have, and the precision is the
thing that changes the answers they get.

## Gate: is this tier wanted at all?

Unlike [[022-model-qwen3.8-27b]], which lands clean, this endpoint is a
product decision before it is an engineering one. It offers a much
larger model at a precision below what its own training targeted, on a
node it fully occupies.

**This spec must not be dispatched until that call is made.** The
engineering below is correct either way; the question is whether a
reduced-precision tier belongs in the registry. If the answer is no, the
GB10 node serves [[022-model-qwen3.8-27b]] and this spec is closed
unimplemented rather than left open.

## Facts (verified 2026-08-28)

- Base checkpoint `deepseek-ai/DeepSeek-V4-Flash-0731`, 304B total /
  13B active MoE, MIT. Per [[017-model-deepseek-v4-flash-0731]]: FP4
  experts, FP8 attention/dense, 166.9 GB, 1M context.
- **The checkpoint is quantization-aware trained with routed experts
  stored natively in MXFP4** — roughly 4 bits. This is commonly cited as
  licence to go lower. It is not: QAT makes the model robust *at its
  native 4 bits*. Every step below that is ordinary quantization loss,
  and 2-bit is two steps below. This cuts against the smallest quants,
  not for them.
- GGUF conversions: `unsloth/DeepSeek-V4-Flash-0731-GGUF`, revision
  `fbbb5b93fb787c21338159b0af3318bb3f4d9768`, MIT.

| Variant | Size | Fits ~115 GB usable |
|---|---|---|
| UD-IQ1_S | 82.5 GB | yes |
| UD-IQ1_M | 86.9 GB | yes |
| UD-IQ2_XXS / UD-IQ2_M | 90.9 GB | yes |
| UD-Q2_K_XL | 96.8 GB | yes |
| **UD-IQ3_XXS** | **104 GB** | **yes, ~11 GB spare** |
| UD-IQ3_S | 116 GB | no |
| UD-Q3_K_M / UD-Q3_K_XL | 128 GB | no |
| UD-IQ4_XS / UD-IQ4_NL | 137 GB | no |
| UD-Q4_K_XL | 155 GB | no |
| UD-Q8_K_XL | 162 GB | no |

- The **DSpark draft head is a separate file**: 10.9 GB at Q8_0, 11.3 GB
  at BF16. It is additive to every row above.

## Quantization decision

Two configurations fit, and they trade quality against speed:

```
A   UD-IQ3_XXS  104.0 GB,  no draft head    → ~11.0 GB for KV + activations
B   UD-IQ2_M     90.9 GB + 10.9 GB draft    → ~13.2 GB for KV + activations
```

**Default is A.** The draft head buys throughput, not quality, and the
QAT reasoning above says the 3-bit step is where the weights are still
close to what the model was trained to tolerate. An endpoint whose only
reason to exist is "a bigger model" should not spend its quality budget
on decode speed.

**A is conditional on one number this spec does not have.** Qwen's KV
cache is computable from its published head geometry
([[022-model-qwen3.8-27b]]); V4-Flash's is not stated in sources checked
here. If measured KV at the target context exceeds ~11 GB, A does not
fit and the choice becomes B, or A at a reduced `--ctx-size`.

Resolving this is AC1, and it gates the manifest. Do not pin a quant
before measuring.

Note that 1M context is not on offer at either configuration. This tier
serves a context ceiling set by what is left after the weights, and
`context_max` states that honestly rather than inheriting 017's value.

## Provenance

Unlike [[022-model-qwen3.8-27b]], converting this ourselves is not
affordable — a 284B imatrix quantization is its own compute project. So
`hf_repo` is the **third-party GGUF repo**, pinned at the SHA above, and
the freeze rules from [[021-local-weight-loading]] apply to it as the
source of record.

`license_note` must say plainly that these weights are a third-party
requantization of an MIT vendor checkpoint, not the vendor's own
artifact. That is a different trust position from every other model in
the registry and the manifest is where it is recorded.

## Speculative decoding is not DSpark here

017 uses SGLang's `--speculative-algorithm DSPARK`, which reads the
draft head out of the target checkpoint and forbids a separate draft
path. llama.cpp does generic speculative decoding via `--model-draft`
pointing at a **separate GGUF file**, which is the exact shape
`validateDSpark` rejects.

[[020-llamacpp-runtime]] AC3 scopes that validation to `runtime: sglang`
for this reason. Without it, configuration B cannot be expressed.

## Acceptance criteria

- **AC1** KV cache at the intended `--ctx-size` is measured on the GB10
  node and recorded in this spec, and the A/B choice above is resolved
  against it before the manifest is pinned.
- **AC2** `models/deepseek-v4-flash-0731-q2.yaml` validates with
  `runtime: llamacpp`, `load: local`, `gpu: {type: gb10, count: 1,
  nodes: 1}`, and a `context_max` that reflects the measured budget
  rather than 017's 1M.
- **AC3** The endpoint is registered in Lux under
  `deepseek-v4-flash-0731-q2` and a test asserts it is **not** aliased
  to, or routed as a fallback for, `deepseek-v4-flash-0731`.
- **AC4** `license_note` records the third-party requantization, and
  README's table shows the tier and its GPU class as a distinct row.
- **AC5** A quality comparison against 017's endpoint on the coding and
  agentic axis 017 names, published with the endpoint. A reduced tier
  without a measured gap is an unquantified downgrade.
- **AC6** If configuration B is chosen, the draft file is covered by the
  same `_manifest.json` and a llamacpp manifest carrying `--model-draft`
  validates.

## Out of scope

- Producing our own imatrix quantization of the base checkpoint.
- Serving 1M context on this class.
- Retiring 017. The B200 endpoint is the reference tier and this one
  does not replace it.
