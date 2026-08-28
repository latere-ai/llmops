---
title: "arm64 runtime images"
status: draft
depends_on:
  - 003-serving-runtime.md
  - 019-gb10-serving-target.md
affects:
  - Dockerfile.sglang
  - Dockerfile.vllm
  - Dockerfile.mirror
  - Makefile
  - DEPLOY.md
effort: small
created: 2026-08-28
updated: 2026-08-28
author: changkun
dispatched_task_id: null
---

# arm64 runtime images

## Problem

[[019-gb10-serving-target]] adds an arm64 node class. The **engines are
already there** — `lmsysorg/sglang:v0.5.16-cu129` and
`vllm/vllm-openai:v0.28.0` both publish `linux/arm64`, and vLLM builds
SM121 kernels. No new engine is needed and
[[001-inference-engine-selection]] stands unamended.

What is not there is **our own layer on top of them**. Three things in
this repo assume amd64, and each one silently produces a broken or
absent arm64 image rather than a build failure that names the cause.

### 1. s5cmd is downloaded as an amd64 binary

Every runtime Dockerfile fetches the same asset:

```
s5cmd_2.3.0_Linux-64bit.tar.gz     # amd64 only
```

Upstream publishes `s5cmd_2.3.0_Linux-arm64.tar.gz` alongside it. On an
arm64 build the amd64 binary installs without error and fails at weight
fetch time — which is after the image is published, after the pod
starts, and inside the `nvme-cache` path that every model depends on.

### 2. `make release` pins the platform

```make
docker build --platform linux/amd64 -f Dockerfile.sglang ...
```

All four images are built `linux/amd64` unconditionally, so an arm64
image is never produced at all.

### 3. The Go stage builds for the build host

`CGO_ENABLED=0 go build` targets the builder's architecture. Under
`--platform` the base image changes but `GOARCH` does not follow unless
it is set, so a cross-build silently emits an amd64 binary into an arm64
image.

## Decision

**Use Docker's built-in `TARGETARCH`, and build the release images with
`buildx` for both platforms.**

`TARGETARCH` is populated automatically by BuildKit (`amd64`, `arm64`),
so no new build arg is introduced and the same Dockerfile serves both.

```dockerfile
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -o /out/runtime ./cmd/runtime
```

```dockerfile
ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      amd64) S5=Linux-64bit ;; \
      arm64) S5=Linux-arm64 ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://github.com/peak/s5cmd/releases/download/v2.3.0/s5cmd_2.3.0_${S5}.tar.gz" \
       | tar -xz -C /usr/local/bin s5cmd
```

The explicit `exit 1` on an unknown architecture is the point of the
`case`: a new platform must fail the build rather than inherit amd64.

Release builds become multi-arch:

```make
docker buildx build --platform linux/amd64,linux/arm64 --push ...
```

### The K3 image stays amd64

`open-llms-runtime-sglang-k3` pins a base tag that is itself
amd64-specific (`lmsysorg/sglang:kimi-k3-…-amd64`), and Kimi-K3 needs a
GPU pool that has nothing to do with this class. It is built
single-platform on purpose, not by omission.

## Verification gap

vLLM's build config lists compute capability 12.1, and SGLang publishes
an arm64 tag. Neither fact proves the **published arm64 binary** ships
SM121 kernels — the arm64 build could target only the server-class Grace
parts. This is cheap to settle and expensive to assume, so it is AC1
rather than a footnote.

## Acceptance criteria

- **AC1** The upstream arm64 images are confirmed to run on SM121 before
  any GB10 model spec is dispatched: pull each image on a GB10 node,
  start the engine on a small model, and record which engine and tag
  worked. If neither does, this spec grows a source-build stage and
  [[019-gb10-serving-target]]'s "no new engine" claim is revisited.
- **AC2** `make images` and `make release` produce working `linux/arm64`
  images for `sglang`, `vllm` and `mirror`; `sglang-k3` stays amd64.
- **AC3** The `runtime` binary inside an arm64 image reports arm64, with
  a test that would fail on the current `GOARCH`-unset build.
- **AC4** s5cmd inside an arm64 image executes and completes a real
  fetch — not merely present on the filesystem, which is what the
  current bug looks like.
- **AC5** An unknown `TARGETARCH` fails the build with a message naming
  the architecture.
- **AC6** DEPLOY.md documents that release images are multi-arch and
  which one is not.

## Out of scope

- Adding a third engine. Both incumbents run here; see
  [[019-gb10-serving-target]].
- Bumping SGLang. Its pinned tag already publishes arm64, and
  [[001-inference-engine-selection]] says bump deliberately.
- arm64 CI runners. Building multi-arch under emulation is acceptable
  for images this size; revisit if build time becomes the constraint.
