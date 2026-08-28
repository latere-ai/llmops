---
title: "Model: DeepSeek-V4-Flash-0731 on GB10 (reduced-precision tier)"
status: draft
depends_on:
  - 017-model-deepseek-v4-flash-0731.md
  - 019-gb10-serving-target.md
  - 020-arm64-runtime-images.md
  - 021-local-weight-loading.md
affects:
  - models/deepseek-v4-flash-0731-q2.yaml
  - deploy/deepseek-v4-flash-0731-q2/
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
router includes removing "silent model swaps". Serving both under one
model id would be exactly that swap, performed by us.

**Decision: a separate, separately-named endpoint. The two are never
routed under one Lux model id, and the name states the tier.**

```
deepseek-v4-flash-0731       FP4/FP8, 8x B200   (017)
deepseek-v4-flash-0731-q2    ~2 bit, 1x GB10    (this spec)
```

Named for the quantization, not the hardware: the GPU is an
implementation detail a caller does not have; the precision changes the
answers they get.

## Gate: is this tier wanted at all?

Unlike [[022-model-qwen3.8-27b]], which lands clean, this endpoint is a
product decision before it is an engineering one. It offers a much
larger model below the precision its own training targeted, on a node it
fully occupies.

**Do not dispatch this spec until that call is made.** If the answer is
no, the GB10 node serves [[022-model-qwen3.8-27b]] and this spec is
closed unimplemented rather than left open.

## Facts (verified 2026-08-28)

- Base checkpoint `deepseek-ai/DeepSeek-V4-Flash-0731`, 304B total /
  13B active MoE, MIT. Per [[017-model-deepseek-v4-flash-0731]]: FP4
  experts, FP8 attention/dense, 166.9 GB, 1M context.
- **The checkpoint is quantization-aware trained with routed experts
  stored natively in MXFP4** — roughly 4 bits. This is commonly read as
  licence to go lower. It is not: QAT makes the model robust *at its
  native 4 bits*. Every step below is ordinary quantization loss. This
  cuts against the smallest quants, not for them.
- The vendor checkpoint at 166.9 GB does not fit 128 GB, and neither
  would a 4-bit AWQ/GPTQ requantization at roughly 150 GB. **Sub-4-bit
  is required**, and in practice that means GGUF.
- GGUF conversions: `unsloth/DeepSeek-V4-Flash-0731-GGUF`, revision
  `fbbb5b93fb787c21338159b0af3318bb3f4d9768`, MIT.
- The **DSpark draft head is a separate file**: 10.9 GB at Q8_0,
  11.3 GB at BF16. Additive to every row below.

## What fits

[[019-gb10-serving-target]] AC2 caps the engine's memory fraction at
0.80 of the unified pool, so the engine may address about **102 GB** —
not the full 115 GB. That ceiling, not the raw pool, is the constraint:

| Variant | Size | + draft | Under 102 GB |
|---|---|---|---|
| UD-IQ1_S | 82.5 GB | 93.4 GB | yes |
| UD-IQ2_XXS / UD-IQ2_M | 90.9 GB | 101.8 GB | yes, barely with draft |
| UD-Q2_K_XL | 96.8 GB | 107.7 GB | alone only |
| UD-IQ3_XXS | 104 GB | 114.9 GB | **no** |
| UD-IQ3_S and above | ≥116 GB | — | no |

The 0.80 ceiling rules out the 3-bit quants entirely. That ceiling is
derived rather than measured ([[019-gb10-serving-target]] AC3a); if
measurement raises it, UD-IQ3_XXS comes back into range and this table
is recomputed.

## Quantization decision

Two configurations fit today:

```
A   UD-Q2_K_XL   96.8 GB, no draft head   -> best weights that fit alone
B   UD-IQ2_M     90.9 GB + 10.9 GB draft  -> speculative decoding, 101.8 GB
```

**Default is A**, on the same reasoning that rules out going lower than
necessary: the draft head buys decode speed, not quality, and an
endpoint whose only justification is "a bigger model" should spend its
budget on weights. B is the choice if measured throughput on A is too
low to be useful, which is a real possibility worth testing before
settling.

Neither configuration offers 1M context. `context_max` states what is
left after the weights, rather than inheriting 017's value.

## Engine and the open technical risk

**vLLM**, per [[020-arm64-runtime-images]] — no new engine is needed for
this class. vLLM's GGUF support covers the imatrix types (IQ1_M, IQ1_S,
IQ2_XXS, IQ2_XS, IQ2_S, IQ3_XXS, IQ3_S, IQ4_XS, IQ4_NL) and the k-quants
(Q2_K through Q6_K), so both configurations are expressible.

**The risk is coverage, not format.** vLLM supporting a quantization
*type* is not the same as vLLM running a 304B MoE with this
architecture from GGUF, and its GGUF path is less exercised than
llama.cpp's. This is the single thing most likely to sink the spec, so
it is AC1.

Speculative decoding, if configuration B is chosen, is vLLM's
`--speculative-config '{"method":"dspark"}'` (≥0.27, and the image is
now v0.28.0). Whether that path accepts a **separate GGUF draft file**
rather than an in-checkpoint head is unverified and is part of AC1.

## Provenance

Unlike [[022-model-qwen3.8-27b]], converting this ourselves is not
affordable — a 284B imatrix quantization is its own compute project. So
`hf_repo` is the **third-party GGUF repo**, pinned at the SHA above, and
[[021-local-weight-loading]]'s freeze rules apply to it as the source of
record.

`license_note` must say plainly that these weights are a third-party
requantization of an MIT vendor checkpoint, not the vendor's own
artifact. That is a different trust position from every other model in
the registry, and the manifest is where it is recorded.

## Acceptance criteria

- **AC1** vLLM on the arm64 image loads this checkpoint from GGUF on a
  GB10 node and answers correctly, before anything else here is built.
  If it does not, this spec either grows a llama.cpp engine — reopening
  what [[019-gb10-serving-target]] closed — or is abandoned. Settle this
  first; everything below is wasted if it fails.
- **AC2** KV cache at the intended context is measured and recorded
  here, and the A/B choice is resolved against it before the manifest is
  pinned.
- **AC3** `models/deepseek-v4-flash-0731-q2.yaml` validates with
  `runtime: vllm`, `load: local`, `gpu: {type: gb10, count: 1, nodes: 1}`,
  a memory fraction within the 0.80 bound, and a `context_max`
  reflecting the measured budget rather than 017's 1M.
- **AC4** The endpoint registers in Lux as `deepseek-v4-flash-0731-q2`,
  and a test asserts it is **not** aliased to, or routed as a fallback
  for, `deepseek-v4-flash-0731`.
- **AC5** `license_note` records the third-party requantization, and
  README shows the tier and its GPU class as a distinct row.
- **AC6** A quality comparison against 017's endpoint on the coding and
  agentic axis 017 names, published with the endpoint. A reduced tier
  without a measured gap is an unquantified downgrade.

## Out of scope

- Producing our own imatrix quantization of the base checkpoint.
- Serving 1M context on this class.
- Retiring 017. The B200 endpoint is the reference tier; this does not
  replace it.
