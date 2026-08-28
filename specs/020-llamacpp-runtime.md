---
title: "llama.cpp runtime (GGUF engine)"
status: draft
depends_on:
  - 001-inference-engine-selection.md
  - 003-serving-runtime.md
  - 019-gb10-serving-target.md
affects:
  - internal/manifest/
  - internal/runtime/serve.go
  - Dockerfile.llamacpp
  - models/
effort: medium
created: 2026-08-28
updated: 2026-08-28
author: changkun
dispatched_task_id: null
---

# llama.cpp runtime (GGUF engine)

## Decision

**Add `runtime: llamacpp` as a third engine, GGUF-only, built for
aarch64 + SM121.** This amends [[001-inference-engine-selection]], which
picked SGLang primary and vLLM second and explicitly deferred any third
engine. The deferral held while every node was x86_64 with discrete HBM.
[[019-gb10-serving-target]] ends that.

## Why a third engine rather than a port

Three independent reasons, any one of which is sufficient:

1. **No aarch64 SM121 build of either incumbent exists** at the pinned
   versions, and the community aarch64 SGLang builds that do exist sit
   below the v0.5.16 floor [[017-model-deepseek-v4-flash-0731]] needs.
   Porting means owning a PyTorch-for-arm64-CUDA-13 build, which is a
   larger commitment than adding an engine.
2. **llama.cpp builds natively on aarch64 + CUDA 13** with no Python or
   PyTorch in the dependency path at all. The CGO-free Go entrypoint and
   a C++ engine is a materially smaller image than either incumbent.
3. **Only llama.cpp has sub-4-bit GGUF quantization.** That is not a
   nice-to-have on this class: it is the difference between a 284B model
   fitting in 128 GB and not fitting. See
   [[023-model-deepseek-v4-flash-0731-gb10]].

The shim is unaffected. `llama-server` serves OpenAI-compatible
`/v1/chat/completions`, so the passthrough and the
`llmdialect` Anthropic translation in [[003-serving-runtime]] work
unchanged. This is the whole reason a third engine is cheap.

## Schema changes

### Runtime enum

```go
RuntimeLlamaCpp = "llamacpp"
```

Validated exactly like `sglang` and `vllm`: `image` forbidden, `args`
required. `EngineImage()` returns `open-llms-runtime-llamacpp`.

### Weights are a file, not a directory

`PrepareWeights` returns a *directory* today, and both incumbent engines
take a directory as `--model-path`/positional. llama.cpp takes a single
`.gguf` **file**, and multi-shard GGUF repos publish
`<name>-00001-of-000NN.gguf` with the engine discovering the rest from
the first shard.

Add one optional manifest field, required when `runtime: llamacpp`:

```yaml
weights_file: DeepSeek-V4-Flash-0731-UD-IQ2_M-00001-of-00003.gguf
```

It is a path relative to the prepared weights directory. Validation:
must be non-empty and end in `.gguf` for llamacpp; must be absent for
every other runtime. Resolving it against the prepared directory keeps
`PrepareWeights` unchanged and keeps the "engine gets a path" contract
in one place.

### Vision projector

Multimodal GGUF ships the vision tower as a separate projector file.

```yaml
mmproj_file: mmproj-Qwen3.8-27B-f16.gguf
```

Optional, llamacpp-only, same relative-path rule. Present → the engine
command gains `--mmproj <resolved>`. Absent → the model is text-only
even if the checkpoint has a vision tower, which is a real and silent
failure mode, so [[022-model-qwen3.8-27b]] asserts on it.

### DSpark validation must not fire here

`validateDSpark` keys on `--speculative-algorithm=DSPARK` and enforces
SGLang's constraints (`pp_size == 1`, DP attention off, no separate
draft path). Those are **SGLang flag semantics**, not model semantics.
llama.cpp does generic speculative decoding through `--model-draft` with
a separate draft file, which is the exact shape `validateDSpark`
currently rejects.

Scope the check to `runtime: sglang`. Without this, the GB10 DeepSeek
manifest either fails validation or is forced into a false shape.

## Engine command

```
llama-server --model <weights_file> --host 0.0.0.0 --port <port>
             --alias <manifest name>
             [--mmproj <mmproj_file>] [args...]
```

`--alias` is llama.cpp's equivalent of the `--served-model-name` flag
the other two engines take, and the shim's model-id contract depends on
it: callers address `open-llms/<name>` and the engine 404s an unknown
id. Recent llama.cpp builds also accept `--served-model-name`; confirm
which the pinned build honours at implementation time and use one, not
both.

Flags a GB10 manifest will carry in `args`, none of them defaulted by
the runtime because they are model decisions:

| Flag | Why it is per-model |
|---|---|
| `-ngl` / `--n-gpu-layers` | Offload count. On unified memory this is normally all layers, but it is the memory-budget knob. |
| `-c` / `--ctx-size` | KV cache size, the second term in the [[019-gb10-serving-target]] budget formula. |
| `--model-draft` | Speculative decoding draft file, when the model ships one. |
| `-fa` / `--flash-attn` | Not universally supported per architecture. |

## Image

`Dockerfile.llamacpp`, built `linux/arm64`, CUDA 13 base, compiled with
`-DGGML_CUDA=ON` and the SM121 architecture pinned. Same two-stage shape
as the existing Dockerfiles: `golang:1.27` builds the CGO-free
`runtime` binary, the engine stage carries it, `ENTRYPOINT ["runtime",
"serve"]`.

The engine version is pinned as deliberately as the other two. llama.cpp
has no stable release cadence, so the pin is a commit SHA with the
reason recorded inline, matching how `Dockerfile.sglang` records its
v0.5.16 floor.

The image sets `OPENLLMS_ENGINE_HEALTH_PATH=/health`, which the shim
already reads — `llama-server` exposes `/health`, not the `/ping` or
`/health_generate` the incumbents use. No shim change.

## Acceptance criteria

- **AC1** `runtime: llamacpp` validates with `args` and `weights_file`
  set, and is rejected when `weights_file` is missing, does not end in
  `.gguf`, or is set on a non-llamacpp runtime.
- **AC2** `EngineCommand` renders the `llama-server` line above,
  resolving `weights_file` and `mmproj_file` against the prepared
  directory, with `--alias` set to the manifest name.
- **AC3** `validateDSpark` no longer fires for non-sglang runtimes, and
  a test pins that a llamacpp manifest with `--model-draft` validates.
- **AC4** `Dockerfile.llamacpp` builds for `linux/arm64` and the image
  serves a GGUF model end to end under `make e2e`, using the existing
  in-process fakes rather than a GPU.
- **AC5** `EngineImage()` returns `open-llms-runtime-llamacpp`, and
  `deploycheck` passes for a gb10 LWS referencing it.
- **AC6** The shim's OpenAI passthrough and `/anthropic/v1/messages`
  translation are exercised against `llama-server`'s response shape,
  including a streamed tool call, with no shim code change.

## Out of scope

- llama.cpp on the x86_64 fleet. The incumbents are better there and
  [[001-inference-engine-selection]]'s reasoning is unchanged.
- GGUF conversion. Producing a GGUF from a vendor checkpoint is
  [[022-model-qwen3.8-27b]]'s problem, not the runtime's.
- `--model-draft` tuning. Adding the flag is here; choosing draft
  lengths is per-model.
