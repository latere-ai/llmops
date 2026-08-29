# Models

The models Latere serves with llmops — several still blocked, as the
notes column says. Nothing here is a property of the tool: a model is a
manifest under [`models/`](../models), and the registry is whatever set
of manifests a deployment checks in.

**Deploy** is how each one is run *today*, not a restriction on it. The
mode is a manifest field, and deploy mode and hardware are independent
axes. A GB10 box can join a cluster; an H200 host can be run bare-metal
for a one-off. Switching a model is a one-line manifest edit plus the
artifact that mode uses.

| Model | Vendor | Class | Hardware | Deploy | Notes |
|---|---|---|---|---|---|
| MiniMax M3 | MiniMax | MoE | 8x H200 | k8s | |
| GLM-5.2 | Zhipu AI | MoE | 8x H200 | k8s | |
| Kimi K2.7 Code | Moonshot AI | MoE | 8x H200 | k8s | |
| DeepSeek V4 Pro | DeepSeek | MoE | 8x B200 | k8s | blocked on hardware |
| DeepSeek V4 Flash 0731 | DeepSeek | MoE | 8x B200 | k8s | speculative decoding (DSpark) |
| Kimi K3 | Moonshot AI | MoE | 8x B300 | k8s | multimodal; blocked on hardware + license |
| Qwen3.8-27B | Alibaba | dense | 1x GB10 | bare-metal | multimodal; BF16, no quantization |

Exact revisions, weight formats, and GPU footprints are pinned per model
in [`specs/`](../specs/README.md) and in each model's manifest under
[`models/`](../models). "Blocked" means the weights are frozen and the
manifest is checked in, but no endpoint exists yet — see the model's
spec for what the block is.

Qwen3.8-27B is the one that is not frontier-scale MoE, on purpose. The
registry serves what we choose to own, and a model small enough to run
undamaged on a single GPU is a different product from a large one
squeezed to fit — not a lesser version of one.

## What each endpoint answers

Every model, in either deploy mode, serves the same surfaces on one
port. The caller dialects do not vary per model; what varies is which
one the engine speaks natively.

| Path | Dialect | Notes |
|---|---|---|
| `/v1/chat/completions` | OpenAI Chat Completions | native for both shipped engines |
| `/v1/messages` | Anthropic Messages | the path an unmodified Anthropic SDK requests |
| `/v1/responses` | OpenAI Responses | |
| `/healthz`, `/ready` | — | `/ready` waits for verified weights *and* engine health |
| `/metrics` | — | engine Prometheus output plus `llmops_*` |

A caller dialect that matches the engine's own is proxied untouched. The
rest translate through [`latere.ai/x/pkg/llmdialect`](https://github.com/latere-ai/pkg),
which reports every request field the translation could not carry. That
report is returned in the `X-LLMOps-Compat-Loss` header and counted in
`llmops_dialect_loss_total`, so a lossy pairing is visible rather than
silent. The engine's own dialect is declared per manifest in
`engine_dialect` and defaults to `openai-chat`.

The Lux dialect is deliberately not served here. It belongs to the
gateway, which embeds the same translator package.

## Adding a model

1. Write `models/<name>.yaml`: pinned `hf_repo` + `revision`, weight
   `format`, `runtime`, `gpu`, `context_max`, `deploy`, `load`.
2. Freeze the weights — [deploy guide](./deploy.md), or `llmops pull` +
   `llmops freeze` for a host that serves from its own disk.
3. Add the deploy artifact the mode owns: a LeaderWorkerSet under
   `deploy/<name>/` for k8s, or the unit `llmops install` generates for
   bare-metal.
4. `llmops validate models/` — it checks the manifest *and* that the
   artifact matches it. CI runs the same check.

Whether the model belongs on that GPU at all is a separate question:
[sizing a model for a GPU](./practice.md) is how to answer it before
spending the download.
