# llmops specs

Design records for llmops: how open-weight models are frozen, served,
deployed and measured, and why each choice was made that way.
Start with [000-architecture](000-architecture.md) (umbrella), then read in
number order — the numbering is the implementation order.

**Status is what the code says, not what the spec hoped.** `complete`
means every acceptance criterion holds; `built` means it is in use with a
named criterion still open, listed under the table; `draft` means nothing
of it is in the product.

| # | Spec | Status | Scope |
|---|---|---|---|
| 000 | [Architecture (umbrella)](000-architecture.md) | draft | Goals, planes, constraints, roadmap |
| 001 | [Inference engine selection](001-inference-engine-selection.md) | draft | Decision record: SGLang primary, vLLM second |
| 002 | [Frozen weights registry](002-weights-registry.md) | draft | HF → S3 mirror tool, manifests, initial ~2.7 TB set |
| 003 | [Serving runtime](003-serving-runtime.md) | draft | Engine containers, S3/NVMe weight loading, health contract |
| 004 | [Model: Kimi-K2.7-Code](004-model-kimi-k2.7-code.md) | draft | First model live (INT4, 8x H200, single node) |
| 005 | [Model: GLM-5.2](005-model-glm-5.2.md) | draft | FP8 + expert-parallel path, 1M context |
| 006 | [Model: MiniMax-M3](006-model-minimax-m3.md) | draft | MXFP8, sparse-attention flags, license gate |
| 007 | [Model: DeepSeek-V4-Pro](007-model-deepseek-v4-pro.md) | draft | Largest; hardware-gated (Blackwell / H200 / 2-node) |
| 008 | [Kubernetes GPU serving](008-k8s-serving.md) | draft | LWS deploys, NVMe prefetch cache, node prereqs |
| 009 | [Lux integration](009-lux-integration.md) | draft | Models as Lux providers, cost tracking |
| 010 | [Observability & bench](010-observability-bench.md) | draft | Metrics/dashboards, benchmark harness, router comparison |
| 011 | [Local full-stack e2e](011-local-e2e.md) | draft | Small model + MinIO + local engine; zero-cost pipeline proof |
| 012 | [Lens fitting pipeline](012-lens-fitting-pipeline.md) | draft | Jacobian lens artifacts per model+revision; first Python code |
| 013 | [In-engine jspace capture](013-inengine-capture.md) | draft | vLLM plugin + SGLang patch; on-GPU lens apply, all decode tokens |
| 014 | [jspace readout API](014-jspace-readout-api.md) | draft | Shim SSE stream + Prometheus aggregates from capture frames |
| 015 | [jspace inspector + dashboards](015-jspace-dashboard.md) | draft | Live layer×token grid at /jspace/ui; Grafana dashboard |
| 016 | [Fleet MoE lens fitting](016-bigmodel-lens-fitting.md) | draft | Cost-gated VJP fitting for 1T-class models; deferred |
| 017 | [Model: DeepSeek-V4-Flash-0731](017-model-deepseek-v4-flash-0731.md) | draft | Answers 007 AC1; first speculative decoding (DSpark), 8x B200 |
| 018 | [Model: Kimi-K3](018-model-kimi-k3.md) | draft | 2.8T multimodal; new B300 pool, K3-only image, MaaS license gate |
| 019 | [GB10 serving target](019-gb10-serving-target.md) | **built** | Single-GPU unified-memory lab box; memory budget, one model per host |
| 020 | [Bare-metal deploy mode](020-bare-metal-packaging.md) | **built** | Installed binary + systemd beside k8s; `llmops install` |
| 021 | [Local weight loading](021-local-weight-loading.md) | **complete** | `load: local`, verify in place, no S3 required |
| 022 | [Model: Qwen3.8-27B](022-model-qwen3.8-27b.md) | **built, serving** | BF16 multimodal on 1x GB10; measured 3.0 tok/s |
| 023 | [Model: DeepSeek-V4-Flash-0731 (GB10)](023-model-deepseek-v4-flash-0731-gb10.md) | draft | Reduced-precision tier, separate endpoint; gated on a product call |
| 024 | [One binary: the llmops command](024-single-cli.md) | **complete** | Three binaries collapse into ten flat `llmops` subcommands |
| 025 | [Dialect surfaces](025-dialect-surfaces.md) | **complete** | All three caller dialects; engine dialect declared, loss reported |
| 026 | [Harness integration](026-harness-integration.md) | **complete** | `ps`, `endpoint --harness`, `run` — the last mile to coding against it |
| 027 | [Qwen fast path](027-qwen-fast-path.md) | draft | NVFP4 + speculative decoding: 3 tok/s measured, ~50 available |

### What is built, and what each built spec still owes

| # | Open criterion |
|---|---|
| 019 | AC4 — the 26 GB host reserve behind the 0.80 memory cap is unmeasured under load, and [027](027-qwen-fast-path.md) reports someone running 0.90–0.95 on this hardware. AC7/AC8 — the deploy guide does not describe the gb10 pool, and [010](010-observability-bench.md) does not record that device-memory metrics are absent on this class. |
| 020 | AC6 — no end-to-end test covers install → serve → `/ready` → completion. |
| 022 | AC4 — **no 262K-token request has been sent.** The cache holds 292,125 tokens, but capacity is not a served request. |

Everything else those specs asked for holds, and 021, 024, 025 and 026
are closed.

Dependency shape: 001 and 002 unblock everything; 003 needs both. A
model spec needs 003 plus the deploy mode it uses — 008 for the fleet,
[020](020-bare-metal-packaging.md) for a single-GPU host — which is the
half 012 through 018 could assume and 022 could not. 004 (Kimi) was
deliberately first: the cheapest full-quality path to prove S3 → engine →
k8s → Lux end to end. 007 (DeepSeek-V4-Pro) is last, the only model that
may not fit a single existing node.

017 and 018 are the 2026-08 model refresh. 017 (V4-Flash-0731) resolves
007's escape hatch — a DeepSeek-V4 endpoint on one existing node, at 1/5
the weights, so Pro can stay blocked on procurement without blocking the
vendor. 018 (Kimi-K3) is the opposite: it needs a GPU pool, an engine
image, and a license determination we do not have yet, so it is gated
three ways and mirrored regardless.

019 through 027 are the GB10 track — the first host with one GPU,
unified CPU/GPU memory, an arm64 CPU, and no cluster around it. Most of
it is built and one model is serving on it.

Two things it did **not** change. There is no new engine: both pinned
engine images already publish linux/arm64, and vLLM's sm_120 kernels run
on this GPU by binary compatibility — measured, 019 — so 001's decision
record stands. And the Kubernetes deploy path is untouched: 020 adds a
**second** deploy mode beside it, an installed binary under systemd
selected by a `deploy:` field, because a one-GPU host has nothing for a
scheduler to schedule. Both modes share the manifest schema, the
frozen-weights provenance and the health contract, and `deploycheck`
covers both.

019 states what the hardware class costs, and the cost is memory
behaviour rather than software availability: a memory *fraction* is taken
from the host's RAM too, the engine fills whatever it is given, device
memory is unreadable through `nvidia-smi`, and CPU offload buys nothing
because there is no second pool. 021 drops the S3 requirement so a host
serves from its own disk — the only sensible mode with no object store in
the picture.

**022 is serving, and the interesting part is what measurement changed.**
The memory arithmetic held: KV predicted at 64 KiB/token against 65.4
measured, the engine inside its budget. Throughput did not: **3.0 tok/s**,
which is ~70% of the memory-bandwidth ceiling for reading 54 GB per token
and far too slow to work in. 023 remains gated twice — on whether a
reduced-precision tier belongs in the registry at all, and on whether
vLLM can load a 304B MoE from GGUF.

024, 025 and 026 are complete and came out of using what we already had.
Shipping binaries to a host made three of them a cost the container mode
never paid, so `mirror`, `runtime` and `bench` became one `llmops`
command — sequenced before the first deploy, because changing an
entrypoint is free until something runs on it. 025 found the shim wiring
**two of llmdialect's eight codecs**, and discarding the loss report the
package exists to produce, so a caller asking for logprobs through
Anthropic Messages was told nothing. 026 is the last mile: a model
serving on a host is not yet a model you are using, and the port, the
dialect path, the invented key and the config format are all derivable.
It depends on 025 rather than working around it — once every model serves
all three surfaces on one port, `endpoint` stops being dialect routing
and becomes a table of variable names and formats.

027 puts a price on 022's central argument. Undamaged BF16 weights
measure 3.0 tok/s; the same model on the same box has been measured at
**51.5** with 4-bit weights and a draft head. The quality case for BF16
still stands — it now costs a measured 17x rather than the 4x estimated,
which is high enough that the answer may change. Both endpoints stay,
named by precision, for the reason 023 gives. It also corrects a formula:
throughput is bandwidth over bytes-per-token *times accepted tokens per
step*, which is why the same model measures 51.5 on code and 18.3 on
prose.

The jspace monitoring plane (012→015) proves end to end on the local
Qwen3-0.6B stack before any fleet GPU is spent; 016 gates fleet-model
lens fitting behind an explicit cost model, mirroring 007's
hardware-gating pattern.

Research provenance: facts in 000–018 were verified against primary
sources on **2026-07-18**; the GB10 track (019–027) on **2026-08-29**,
where the numbers are measurements from the box rather than citations.
Each spec carries its own sources.

Re-verify pins at implementation time — engines release weekly, and that
advice earned itself twice this week. Qwen3.8-Flash-Next was read as
unservable because the pinned vLLM answered `Model architectures [...]
are not supported for now`; its vendor recipe names a **minimum engine
version**, so the message meant "your engine is older than this model".
And a throughput ceiling derived from bandwidth was contradicted by a
published 51.5 tok/s on the same hardware, because the ceiling assumed
one token per forward pass and the other configuration used speculative
decoding. **Before concluding a checkpoint cannot be served, check the
vendor's recipe for an engine floor; before concluding a number is
impossible, check what it was measured with.**
