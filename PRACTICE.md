# Sizing a model for a GPU

Two questions decide whether a model belongs on a given machine, and
they have different answers:

1. **Does it fit?** Decided by *total* parameters.
2. **How fast will it be?** Decided by *active* parameters.

Conflating them is the usual mistake. A 300B mixture-of-experts model
with 13B active is harder to fit than a 27B dense model and much faster
once it does.

This document is operator guidance. The per-model decisions live in
`specs/`; this is how to reach one for a machine we have not used yet.

## 1. Does it fit?

```
weights + kv_cache + activations  ≤  fraction × pool
```

`weights` is total parameters × bytes per parameter:

| Precision | Bytes/param | 27B | 300B | 1T |
|---|---|---|---|---|
| BF16/FP16 | 2 | 54 GB | 600 GB | 2 TB |
| FP8 | 1 | 27 GB | 300 GB | 1 TB |
| INT8 | 1 | 27 GB | 300 GB | 1 TB |
| 4-bit | 0.5 | 13.5 GB | 150 GB | 500 GB |
| 2-bit | 0.25 | 6.75 GB | 75 GB | 250 GB |

`kv_cache` is per token, times the context you intend to serve:

```
kv_per_token = layers_with_full_attention
             × kv_heads × head_dim × 2 (K and V) × bytes_per_element
```

Read those from the checkpoint's `config.json`, never from the model's
name. Two things routinely surprise:

- **Hybrid-attention models cache on only some layers.** Qwen3.8-27B has
  64 layers but only 16 use full attention; the other 48 are Gated
  DeltaNet, whose recurrent state is fixed size and does not grow with
  sequence length. That is a 4x reduction in KV, and it is the reason a
  262K context is affordable on one GPU. Predicted 64 KiB/token,
  measured 65.4 — the formula is reliable when you read the real config.
- **Compressed-KV designs** (MLA and friends) break the formula in the
  other direction. If the config does not give you the geometry, measure
  rather than guess.

`activations` is a few GB for a single-stream server; 5 GB was the
measured peak for a 27B. It grows with batch size.

## 2. How fast will it be?

Token generation at batch 1 is memory-bandwidth bound. The model reads
its weights once per token:

```
tok/s  ≈  efficiency × bandwidth / bytes_read_per_token
```

- **Dense model:** `bytes_read` is *all* weights.
- **MoE model:** `bytes_read` is the *active* weights only — the router
  touches a fraction of the experts per token.
- **efficiency** is 60–75% in practice. The rest goes to attention,
  sampling, scheduling, and kernels that are newer and less tuned.

This is why active parameters, not total, set the speed. It is also why
quantization buys throughput proportionally: half the bytes, twice the
tokens.

### Measuring bandwidth, and a trap

Do not use a reduction to measure bandwidth. `tensor.sum()` on this box
reports 164 GB/s and is limited by the reduction, not by memory — it
will convince you the hardware is slower than it is. A copy or an
elementwise op over distinct arrays is the honest measure, and count
bytes actually moved:

```python
import torch, time
n = 200_000_000
a = torch.ones(n, dtype=torch.bfloat16, device="cuda"); b = torch.empty_like(a)
for _ in range(5): b.copy_(a)
torch.cuda.synchronize()
t0 = time.perf_counter(); b.copy_(a); torch.cuda.synchronize()
print("%.0f GB/s" % (n*2*2 / (time.perf_counter()-t0) / 1e9))  # read + write
```

## 3. Unified memory changes the rules

On a part where CPU and GPU share one pool (GB10 and similar), three
things differ from a discrete-HBM node, and each has bitten us:

- **A memory *fraction* is taken from the operating system's memory
  too.** vLLM at `--gpu-memory-utilization 0.30` held 37.6 GB on a
  128 GB box — 0.30 of the *whole pool*, for a model whose weights are
  1.2 GB. The default of 0.90 would leave the host ~13 GB. Cap it: we
  use 0.80, and size each model below that.
- **The engine fills the fraction it is given**, it does not stay under
  it. Asking for less is how you leave the host room.
- **`nvidia-smi` reports no device memory** on this class — `[N/A]` for
  total, free and used. Read host `/proc/meminfo` instead. Anything that
  sizes a cache from device-memory queries reads zero.

## 4. The lab box: measured

One GB10, 128 GB unified LPDDR5X, arm64 host, no cluster.

| | Measured |
|---|---|
| Usable memory | ~115 GB (less with a desktop session) |
| Engine budget at our 0.80 cap | ~102 GB |
| Memory bandwidth | **~230 GB/s** (copy; see the trap above) |
| Inference efficiency | ~70% of that |
| NVMe read | 4.9 GB/s |
| Compute capability | sm_121, running sm_120 kernels by binary compatibility |
| Weight verification | 54 GB in 39 s (~1.4 GB/s hashing) |
| Engine start | 382 s for a 27B; ~10 min to `/ready` |

**Startup is the operational surprise.** systemd's 90 s default start
timeout kills a large model mid-load and then keeps killing it, which
presents as a crash loop rather than a timeout. Generated units set
`TimeoutStartSec=30min`.

## 5. Worked examples

Every model we evaluated for this box, with the reasoning that decided
it. `bytes/token` uses *active* parameters at the serving precision.

| Model | Total | Active | Size to serve | Fits 102 GB? | bytes/token | Expected tok/s |
|---|---|---|---|---|---|---|
| Kimi K3 | 2.8T | 104B | ~1.4 TB @ 4-bit | **no**, 14x over | — | — |
| DeepSeek V4 Pro | 1.6T | 49B | 1.6 TB @ FP8 | **no**, 16x over | — | — |
| Qwen3.8-Flash-Next | 125B (+55B) | 6B | 360 GB @ BF16 | **no**, 3.5x over | — | — |
| DeepSeek V4 Flash | 304B | 13B | 90.9 GB @ IQ2_M | tight, yes | ~4 GB | high — see below |
| **Qwen3.8-27B** | 27B dense | 27B | 54 GB @ BF16 | **yes**, comfortably | **54 GB** | **3.0 measured** |

### What this table shows that the individual decisions did not

**Qwen3.8-27B reads 54 GB per token. DeepSeek V4 Flash at 2-bit reads
about 4 GB.** That is a 13x difference in the quantity that sets speed,
and it runs opposite to the quality argument.

The case for Qwen over the compressed DeepSeek was that undamaged
weights beat a large model squeezed to fit. That argument stands on
quality. It does not stand on throughput, and the gap is not marginal —
it is the difference between a model you wait on and one you converse
with. Anyone re-reading `specs/022` and `specs/023` should weigh both.

### Qwen3.8-27B in detail

Predicted before deployment, then measured:

| | Predicted | Measured |
|---|---|---|
| KV per token | 64 KiB | 65.4 KiB |
| Weights resident | 54.0 GB | 56.0 GiB (incl. non-torch) |
| Activation peak | ~6 GB | 4.9 GiB |
| Engine total | ≤ 83.2 GB | 71.4 GiB |
| Throughput | not predicted | 3.0 tok/s (~70% of the 4.3 ceiling) |

The arithmetic was reliable; the throughput was the thing worth learning
by running it.

### The quantization ladder for a 27B dense model

At the same ~70% efficiency:

| Precision | bytes/token | Expected tok/s |
|---|---|---|
| BF16 | 54.0 GB | 3.0 |
| INT8 | 27.0 GB | ~6 |
| 4-bit | 13.5 GB | ~12 |

INT8 is the interesting middle: half the bytes for a quality cost far
below 4-bit's, on a model with no quantization-aware training to lean
on.

## 6. Before committing to a model

1. **Read `config.json`.** Architecture string, layer types, head
   geometry. The name tells you nothing — Qwen3.8-27B declares
   `Qwen3_5ForConditionalGeneration`, and Qwen3.8-Flash-Next declares
   `Qwen4ExpForConditionalGeneration`, a different family entirely.
2. **Check the engine registry for that exact architecture string**, in
   the version you pin — not the latest release notes. Support for a
   model published on the same day as an engine release is usually not
   in it.
3. **Do the fit arithmetic** before downloading hundreds of gigabytes.
4. **Do the bytes-per-token arithmetic** before promising anyone a
   latency.
5. **Then measure**, and write the number down next to the prediction.
   Both times we did this the fit arithmetic held and the speed was the
   surprise.
