---
title: "Model: Qwen3.8-27B fast path (NVFP4 + speculative decoding)"
status: partial
depends_on:
  - 019-gb10-serving-target.md
  - 020-bare-metal-packaging.md
  - 021-local-weight-loading.md
  - 022-model-qwen3.8-27b.md
affects:
  - models/qwen3.8-27b-fast.yaml
  - deploy/qwen3.8-27b-fast/
  - internal/manifest/
  - internal/runtime/
  - internal/harness/
  - internal/install/
  - cmd/llmops/
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
3.0 tok/s**. Claude Sonnet 5 measures around 95 tok/s. Thirty times
slower is not a tuning complaint; it is the difference between a model
you work in and one you demonstrate.

The same model on the same GB10 class has been measured at **51.5 tok/s**
by [MiaAI-Lab](https://github.com/MiaAI-Lab/Qwen3.8-27B-SGLang-DGX-Spark),
which publishes its configuration and its numbers:

| Probe | DSpark | MTP/EAGLE | DFlash2 |
|---|---|---|---|
| Code | **51.5** | 34.5 | 50.9 |
| Long essay | 18.3 | 24.1 | **25.4** |

The 2.8x spread between code and prose is the useful part of that table.
Memory bandwidth is indifferent to what is being generated, so a gap
that large is draft-token acceptance, not throughput.

## Where the 17x comes from

Two independent multipliers, neither of which 022 uses:

```
BF16, one token per forward pass   54.0 GB/token   →   3.0 tok/s  measured
NVFP4 W4A4                         13.5 GB/token   →  ~12 tok/s   4x fewer bytes
+ speculative decoding                             →  18-51 tok/s draft acceptance
```

The 4-bit step lands on the ~12 tok/s already predicted in
`docs/practice.md`, so nothing here contradicts the bandwidth model.

**It does break the formula that document states.**
`tok/s ≈ efficiency × bandwidth / bytes_per_token` assumes one token per
forward pass. Speculative decoding reads the weights once and verifies
several tokens against that read, so the honest form multiplies by
accepted tokens per step. That correction belongs in `docs/practice.md`
regardless of whether the rest of this ships.

## What this costs

022's argument for this model over the compressed DeepSeek
([[023-model-deepseek-v4-flash-0731-gb10]]) was that undamaged weights
beat a large model squeezed to fit. **This spec spends exactly that.**

NVFP4 is 4-bit weights *and* 4-bit activations: a real quality
reduction, now with a measured price attached. That price is high
enough that the answer may change, which is why this is a separate
endpoint rather than an edit to 022.

**Both endpoints stay.** The same rule as
[[023-model-deepseek-v4-flash-0731-gb10]]: a caller must be able to tell
which one answered, so the precision is in the name.

```
qwen3.8-27b        BF16, undamaged, ~3 tok/s      (022)
qwen3.8-27b-fast   NVFP4 + speculation, ~50 tok/s (this spec)
```

One GPU means one at a time ([[019-gb10-serving-target]]), so this is a
choice made when starting the box, not a routing decision.

## The speculator is configurable, and that is the design

The obvious shape is one manifest per configuration:
`qwen3.8-27b-fast.yaml` for DSpark, another for DFlash2. It fails on the
table above. **DSpark leads on code by 17 tok/s and loses on prose by
7**, so picking one at author time is picking a workload on the
operator's behalf. It fails a second time on licensing: a draft head is
a separate published artifact under its own terms, and near-duplicate
manifests would restate that in three places until one of them drifted.

So a manifest offers a *set*, and the operator chooses when starting:

```
llmops serve --manifest models/qwen3.8-27b-fast.yaml --speculator dflash2
llmops serve --manifest models/qwen3.8-27b-fast.yaml --speculator none
```

`speculators` is a map from name to configuration; `default_speculator`
says which runs when nobody chooses, and is required whenever the map is
non-empty. Which draft head is active changes both throughput and
output, so it is never implicit.

```yaml
default_speculator: dspark
speculators:
  dspark:
    hf_repo: RadixArk/Qwen3.8-27B-DSpark
    revision: b9a5dbdf...
    license: other
    args: [--speculative-algorithm=DSPARK, --speculative-dspark-block-size=7]
  mtp:                                   # in the checkpoint: nothing to fetch
    args: [--speculative-algorithm=EAGLE, --speculative-num-steps=3, ...]
```

Three things follow, and each is a rule in the code rather than a
convention:

**A draft head is an artifact, so it is frozen like one.** An entry
naming an `hf_repo` must pin a 40-hex revision and state its own
license. The DSpark head is licensed `other` while the target it drafts
for is apache-2.0 — a manifest that omitted it would ship a third-party
artifact under the base model's declaration. `llmops freeze` and
`verify` work on it unchanged, because it is the same cache layout.

**The manifest never writes the draft path.** It resolves to
`<cache-root>/<hf_repo>/<revision>` and llmops supplies it at launch,
for the same reason primary weights name no directory
([[021-local-weight-loading]]): an absolute path would pin a checked-in
file to one machine.

**`none` is a real setting, not an absent one.** It serves the
quantized weights with no draft head, which is the only way to separate
what NVFP4 costs in quality from what speculation adds in speed. Two
manifests and one flag then give all three measurement points AC4 needs:
BF16 at 3 tok/s, NVFP4 alone at ~12, NVFP4 with a draft head at ~50.

### Composition, and the two rules it broke

A speculator's flags are appended **after** the model's own so that
several can share one base configuration — and so they can override it.
SGLang's own DFLASH path relies on this: it rejects
`--mamba-radix-cache-strategy extra_buffer_lazy` and needs
`extra_buffer`, which the `dflash2` entry supplies as an override rather
than by forking the base args.

Composition then broke two existing validations, both silently:

1. **The DSpark rule assumed an in-checkpoint head.** It rejected
   `--speculative-draft-model-path` on any manifest naming the
   algorithm, which is right for [[017-model-deepseek-v4-flash-0731]]
   and wrong here — this DSpark head is a separate 2.7 GB repo. Where
   the head lives is a property of the *checkpoint*, not of the
   algorithm. The rule now keys on whether the speculator names an
   `hf_repo`, so 017 keeps its guarantee and this manifest is legal.

2. **Flag lookup returned the first occurrence.** Both engines take the
   last. That was harmless while a manifest had one argument list and
   became a hole the moment two were concatenated: a speculator raising
   `--mem-fraction-static` past [[019-gb10-serving-target]]'s ceiling
   would validate against the base value and starve the host only for
   whoever selected it. Lookup now resolves last-wins, and the ceiling
   is checked against every combination rather than the base args alone.

### Attribution

`--served-model-name` stays `qwen3.8-27b-fast` for every speculator.
Suffixing it would break every caller's configuration whenever an
operator restarted with a different head, and the model being served
really is the same model.

Instead the shim reports `X-LLMOps-Speculator` on every response,
following [[025-dialect-surfaces]]'s loss header: an engine-side fact
the caller cannot otherwise see, carried without touching the payload.
`llmops ps` reads it from the readiness probe and shows a SPECULATOR
column, and `/metrics` exports `llmops_speculator_info` so a throughput
panel can be broken down by it. A number that does not record which
draft head produced it cannot be attributed, and AC3 asks for four.

`llmops install --speculator <name>` pins the choice into the systemd
unit. Without that, a restart would quietly fall back to the manifest's
default.

## Engine and deploy mode: resolved

The earlier draft of this spec called the deploy mode "the one open
question", because the reference runs a model-specific container tag
(`lmsysorg/sglang:qwen38-27b`) and DFlash2 needed a further derived
image. Both concerns are gone, verified 2026-08-29:

- **SGLang installs from PyPI on this architecture.** `sglang==0.5.18`
  and `sglang-kernel==0.4.6.post1` both resolve for aarch64.
- **0.5.18 carries all three algorithms natively.** `DFLASH`, `DSPARK`
  and `EAGLE` are in its `SpeculativeAlgorithm` enum. DFlash2 merged
  upstream on 2026-08-19 and 0.5.18 was released on 2026-08-21; the
  derived image the reference builds predates that release.

So this runs `deploy: bare-metal` like [[022-model-qwen3.8-27b]], with
no container on a box that has no cluster.

**One condition.** Installing SGLang into the venv currently serving
022 downgrades `transformers` 5.16.1 → 5.12.1 and `xgrammar`, which
breaks vLLM. The two engines need **separate virtualenvs**, which
[[020-bare-metal-packaging]]'s single-venv install model does not
describe. That is an amendment to 020, recorded there.

## Configuration this ships with

From the reference, verified against SGLang 0.5.18's own argument
definitions:

```
--attention-backend=flashinfer        required on this compute capability
--kv-cache-dtype=fp8_e4m3             halves 022's measured 18.23 GiB cache
--chunked-prefill-size=8192
--mem-fraction-static=0.80            our ceiling; the reference runs 0.90
--context-length=262144               native; see below
```

Two facts worth not re-learning:

- **YaRN stays off and the context stays native.** Both separately
  published heads read the target's context configuration, and a YaRN
  override leaks into the draft config and crashes the engine at boot.
- **`--speculative-dspark-block-size` sets `--speculative-num-draft-tokens`
  to gamma+1 on its own.** Block 7 is the measured code peak; block 5
  trades 16% of code for 8% of prose.

The target is `RadixArk/Qwen3.8-27B-NVFP4-BF16-LMHead`, not the
packed-FP4 variant. Its dense `lm_head` is what lets DFLASH use its
native selector, and DSpark is indifferent to the choice — so one target
serves all three speculators, which is what makes this one manifest.

## What is built

The schema, the serving path and the operator surfaces are built,
tested and validating. Every package clears the 90% floor.

| | |
|---|---|
| `speculators` / `default_speculator` | schema, validation, resolution |
| draft-head staging | same cache layout, same checksum verification |
| `serve --speculator` | resolved before any weights are touched |
| `install --speculator` | pinned into the unit; unknown names fail before writing |
| `ps` | SPECULATOR column, read from the endpoint |
| `X-LLMOps-Speculator`, `llmops_speculator_info` | attribution |
| `models/qwen3.8-27b-fast.yaml` | three speculators, pinned and licensed |

**Nothing has been served yet.** The weights are not on the box, no
throughput has been measured, and no quality comparison has been run.

## Acceptance criteria

- **AC1** The NVFP4 weights and each separately published draft head are
  pulled, frozen and verified by `llmops freeze`, with `license_note`
  recording the third-party requantization. *Open.*
- **AC2** `models/qwen3.8-27b-fast.yaml` validates and serves under the
  name `qwen3.8-27b-fast`, never aliased to `qwen3.8-27b`. *Validates;
  has not served.*
- **AC3** Measured throughput on our box, by `llmops bench`, for each
  speculator **and** for `none`, on both a code prompt and a prose
  prompt — the two are different numbers and reporting one is
  misleading. Each measurement records the speculator that produced it.
  *Open.*
- **AC4** A quality comparison against the BF16 endpoint on the work
  this is for, with `--speculator none` separating the quantization cost
  from the speculation gain. A 17x speedup at an unmeasured quality cost
  is not a decision, it is a hope. *Open.*
- **AC5** The memory fraction actually used is measured, and
  [[019-gb10-serving-target]] AC2's 0.80 ceiling is either confirmed or
  raised with the measurement behind it. *Open.*
- **AC6** The deploy-mode question is answered in this spec before the
  model ships. **Met** — bare-metal, pip-installed SGLang 0.5.18, one
  venv per engine.
- **AC7** `docs/practice.md`'s throughput formula is corrected to
  account for speculative decoding, and says how to choose between draft
  heads. **Met.**
- **AC8** The DSpark head's `other` license is read before this endpoint
  is exposed beyond the lab. *Open.*

## Out of scope

- Retiring the BF16 endpoint. It is the quality reference this one is
  measured against.
- Producing our own quantization. Published NVFP4 exists; a 27B
  calibration run is its own project.
- Speculative decoding for other models. DeepSeek's case is
  [[023-model-deepseek-v4-flash-0731-gb10]], where the draft head does
  not fit the budget.
- Serving two speculators at once. One GPU, one engine process.
