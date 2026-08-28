---
title: "Bare-metal deploy mode (installed binary + systemd)"
status: draft
depends_on:
  - 003-serving-runtime.md
affects:
  - internal/manifest/
  - internal/deploycheck/
  - cmd/runtime/
  - deploy/
  - Makefile
  - DEPLOY.md
effort: medium
created: 2026-08-28
updated: 2026-08-28
author: changkun
dispatched_task_id: null
---

# Bare-metal deploy mode (installed binary + systemd)

## Decision

**Add a second deploy mode: install the `runtime` binary on a host and
serve a model under systemd, with no container and no Kubernetes.** The
Kubernetes mode is unchanged and remains the fleet path.

The two modes differ only in how the process is started and supervised.
Everything the repo actually owns is shared:

```
models/<name>.yaml   ->   runtime serve --manifest   ->   engine
        ^                          ^
        |                          |
   same schema            same health contract,
   both modes             OpenAI + Anthropic surface
```

A deploy mode is a packaging decision, not a serving decision, and this
spec keeps it that way.

## Why a second mode

The Kubernetes path solves problems a cluster has: scheduling across a
pool, restarting elsewhere on node loss, rolling versions without
downtime, packing models onto shared hardware. A single host with one
GPU has none of those and pays for the scheduler anyway — k3s, a CNI, a
device plugin and an LWS controller, all to supervise one process that
systemd supervises for free.

[[019-gb10-serving-target]] is the first user, but nothing here is
GB10-specific: any single-GPU host without a cluster can use this mode.

## Mode is an explicit manifest field

```yaml
deploy: bare-metal      # or: k8s (default)
```

The alternative is inferring the mode from `gpu.type`, which is wrong on
first principles: deploy mode and hardware are independent axes. A GB10
box could join a cluster; an H200 host could be run bare-metal for a
one-off. Inference would also silently change a model's deploy artifact
when its GPU type changed, which is exactly the class of surprise the
manifest schema exists to prevent.

Validation:

- `deploy` defaults to `k8s`, so every existing manifest keeps its
  meaning with no edit.
- `bare-metal` requires no `image` — there is no container.
- A model may have **one** deploy artifact, never both: `deploy: k8s`
  requires `deploy/<name>/lws.yaml` and rejects a unit file;
  `bare-metal` requires `deploy/<name>/<name>.service` and rejects an
  LWS manifest.

## Artifacts

### The binary

`runtime` is already CGO-free, so it cross-compiles with nothing but
`GOARCH`. `make dist` produces host binaries for the supported pairs:

```
GOOS=linux GOARCH=amd64
GOOS=linux GOARCH=arm64
```

`s5cmd` is **not** a dependency of this mode. It exists in the container
images to sync weights from S3 for `load: nvme-cache`; a bare-metal host
uses `load: local` ([[021-local-weight-loading]]) and needs no object
store client. If a bare-metal host ever wants an S3-backed load mode,
that is when an architecture-specific `s5cmd` download becomes this
spec's problem.

### The unit

`deploy/<name>/<name>.service`, checked in beside the LWS manifests so
both modes' artifacts live in one place:

```ini
[Unit]
Description=llmops <name>
After=network-online.target

[Service]
ExecStart=/usr/local/bin/runtime serve --manifest /etc/llmops/<name>.yaml
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

The engine itself is installed on the host, not vendored here — a pinned
`vllm` or `sglang` in a virtualenv, with the version recorded in
`DEPLOY.md` the way image tags are. The runtime launches whatever
`EngineCommand` resolves to, so this needs no code change.

### The install path

`runtime install --manifest <path>` writes the unit, installs the
manifest to `/etc/llmops/`, and reloads systemd. One command, so the
documented procedure cannot drift from what the binary does — which is
the failure mode a hand-written DEPLOY.md section would have.

## Consistency checking

`deploycheck` asserts manifest-versus-LWS agreement today. It gains a
bare-metal arm asserting manifest-versus-unit: that `ExecStart` names
this model's manifest path and the `runtime` binary.

The check dispatches on `deploy`, so neither mode is validated against
the other's artifact and CI keeps covering both. This is the guarantee
that makes a second mode safe to add rather than a second place for
configuration to rot.

## Acceptance criteria

- **AC1** `deploy: bare-metal` validates; `deploy` absent defaults to
  `k8s` and every existing manifest still passes unchanged.
- **AC2** A model carrying both an LWS manifest and a unit file is
  rejected, in either mode, with an error naming the conflict.
- **AC3** `make dist` builds `linux/arm64` and `linux/amd64` binaries,
  and a test asserts the arm64 binary reports arm64 rather than
  inheriting the builder's architecture.
- **AC4** `runtime install` writes the unit and manifest, is idempotent,
  and a second run over a changed manifest updates rather than
  duplicates.
- **AC5** `deploycheck` validates a bare-metal model against its unit
  file, and a test fails when `ExecStart` names a different manifest or
  binary.
- **AC6** `make e2e` covers the bare-metal path end to end — install,
  serve, `/ready`, a completion, `/metrics` — against the existing
  in-process fakes, with no GPU and no root.
- **AC7** DEPLOY.md documents both modes side by side, including which
  engine versions a bare-metal host must install and that this repo does
  not manage them.

## Out of scope

- Managing the engine install. The host owns its `vllm`/`sglang`
  virtualenv; this repo pins versions in documentation, not in code.
- Packaging as `.deb`/`.rpm`. A binary and a unit file are enough until
  there is a second host to keep in sync.
- Multi-model hosts. [[019-gb10-serving-target]] fixes one model per GPU
  host; lifting that is a separate question.
- Changing the Kubernetes mode in any way.
