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

[[019-gb10-serving-target]] AC2 caps the engine's memory fraction at 0.80
of the unified pool, so the engine may address about **102 GB**. That
budget is not the weight budget: vLLM allocates KV blocks to fill
whatever fraction it is given, and the real constraint is

```
weights  +  kv_cache  +  activations  <=  102 GB
```

**The KV term for this checkpoint is unmeasured.** Qwen's cache is
computable from published head geometry ([[022-model-qwen3.8-27b]]);
DeepSeek does not publish the equivalent for V4-Flash. So the table
below is weights only, and every row must still pay for KV and
activations out of the same 102 GB:

| Variant | Weights | + draft | Left for KV + activations |
|---|---|---|---|
| UD-IQ1_S | 82.5 GB | 93.4 GB | 19.5 / 8.6 GB |
| UD-IQ1_M | 86.9 GB | 97.8 GB | 15.1 / 4.2 GB |
| UD-IQ2_XXS / UD-IQ2_M | 90.9 GB | 101.8 GB | 11.1 / 0.2 GB |
| UD-Q2_K_XL | 96.8 GB | 107.7 GB | 5.2 GB / over budget |
| UD-IQ3_XXS | 104 GB | 114.9 GB | over budget |
| UD-IQ3_S and above | ≥116 GB | — | over budget |

Two things follow, and both are uncomfortable:

- **The 3-bit quants are out entirely**, so the QAT argument against
  going below native 4-bit cannot be honoured at all on this node.
- **The draft head is unaffordable.** Adding 10.9 GB leaves no usable
  KV at any weight size worth serving, so speculative decoding is off
  the table on this node unless the ceiling moves.

The 0.80 ceiling is itself derived rather than measured
([[019-gb10-serving-target]] AC3a). If measurement raises it, this table
is recomputed and the larger quants may return.

## Quantization decision

**Deferred to measurement — this spec does not pin a quant.**

On the numbers above the only candidates with plausible KV headroom are
UD-IQ1_S and UD-IQ2_M, both at or below two bits, on a checkpoint whose
training targeted four. That is a weaker position than the earlier draft
of this spec claimed, and it is material to the gate above: the question
is no longer "a bigger model at some quality cost" but "a bigger model
at one to two bits". AC2 settles the KV number; the gate decision should
be taken with that number in hand, not before.

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

Speculative decoding is not planned here — the draft head does not fit
the budget. If a measured ceiling later makes room, the path is vLLM's
`--speculative-config '{"method":"dspark"}'` (≥0.27, and the image is
now v0.28.0), and whether it accepts a **separate GGUF draft file**
rather than an in-checkpoint head would need verifying first.

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
- **AC2** KV cache per token is measured on a real load and recorded
  here, and a quant is chosen against `weights + kv + activations <=
  102 GB`. If no variant leaves usable context, that is a finding that
  closes this spec, not a reason to raise the fraction.
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
