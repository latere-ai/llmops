# open-llms specs

Design specs for owning the open-weights inference layer end to end.
Start with [000-architecture](000-architecture.md) (umbrella), then read in
number order — the numbering is the implementation order.

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

Dependency shape: 001 and 002 unblock everything; 003 needs both; each
model spec needs 003 + 008; 004 (Kimi) is deliberately first — cheapest
full-quality path to prove S3 → engine → k8s → Lux end to end. 007
(DeepSeek-V4-Pro) is last: it is the only model that may not fit a single
existing node and may force the multi-node path.

The jspace monitoring plane (012→015) proves end to end on the local
Qwen3-0.6B stack before any fleet GPU is spent; 016 gates fleet-model
lens fitting behind an explicit cost model, mirroring 007's
hardware-gating pattern.

Research provenance: facts in these specs (model sizes, engine versions,
framework landscape) were verified against primary sources on 2026-07-18;
each spec carries its own source links. Re-verify pins at implementation
time — engines release weekly.
