---
title: "One binary: the llmops command"
status: draft
depends_on:
  - 003-serving-runtime.md
  - 020-bare-metal-packaging.md
affects:
  - cmd/
  - Makefile
  - Dockerfile.sglang
  - Dockerfile.vllm
  - Dockerfile.mirror
  - deploy/
  - DEPLOY.md
  - README.md
effort: small
created: 2026-08-29
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# One binary: the llmops command

## Decision

**Collapse `mirror`, `runtime` and `bench` into one `llmops` binary with
a flat set of subcommands.**

```
llmops pull      <repo>[@rev] --dir <dir>              fetch weights from HF
llmops freeze    <repo>@<sha> --dir <dir>              write the store manifest in place
llmops push      <repo>@<sha> --dir <dir> --bucket …   upload to an object store
llmops verify    <prefix>                              check a store against its manifest
llmops list      --bucket <root>                       what is mirrored there
llmops serve     --manifest <path>                     engine entrypoint / ExecStart
llmops validate  models/                               manifest + deploy consistency
llmops bench     --url … --model …                     live endpoint benchmark
llmops version                                         what is installed on this host
```

### Why flat rather than a `mirror` group

Grouping the five weight verbs under `mirror` was the obvious first
shape and is the weaker one.

`mirror` names an implementation — an S3 mirror — rather than anything
the caller is trying to do. Worse, it is no longer even accurate:
[[021-local-weight-loading]] added `freeze`, whose whole point is a host
with **no** mirror. A group name that half its members contradict is not
carrying meaning.

`pull` is the most-typed command in the tool, and burying the common
case one level down to keep the rare ones tidy is the wrong trade. Nine
flat verbs is well inside what a CLI carries without grouping.

**One name changes: `ls` becomes `list`.** As `mirror ls` the object was
implied by the group; as a top-level verb `llmops ls` reads as a shell
idiom and says nothing about what is listed. This is the only verb whose
spelling moves, and it moves now because nothing depends on it yet.

Grouping stays available if a genuinely separate object appears later —
the jspace plane in [[014-jspace-readout-api]] is the plausible one. It
would be a group because it acts on something other than weights, which
is the test `mirror` failed.

## Why

**The repo is named for a command that does not exist.** After the
rename there is no `llmops` to type. That is the small reason.

**The bare-metal mode makes it a real cost.**
[[020-bare-metal-packaging]] ships binaries to a host and supervises one
under systemd. Three binaries mean three files to copy, three to version,
three to keep in step, and an install command that has to reason about
which of them a given host needs. One binary makes `llmops install` and
the unit file trivial, and makes "what version is on that box" answerable
with one command rather than three.

**It fixes a real mis-shelving.** `validate` checks manifests and deploy
artifacts. It has nothing to do with the serving runtime and lives under
`runtime` only because that binary existed when it was written. A caller
running `runtime validate models/` on a laptop with no engine and no GPU
is being told something untrue about what the command needs.

**Three binaries link the same packages three times.** The arm64 set is
27.7 MB across `mirror` (9.0), `runtime` (10), and `bench` (8.7), and
they share `internal/manifest` and `internal/mirror` between them. One
binary pays for the shared code once.

## Do it now, or pay for it later

Changing the container `ENTRYPOINT` is a breaking change for anything
already deployed. Nothing is: the README records that the GPU
deployments and gateway registration are not done, and no image tag has
been published. The migration cost today is editing files in this repo.
After the first deploy it becomes a coordinated rollout, and after Lux
registration it becomes one with an audience.

This is the last cheap moment.

## Shape of the change

All three entrypoints already share one signature:

```go
func run(args []string, out, errw io.Writer) int
```

So this is routing, not rewriting. `cmd/llmops/main.go` dispatches on
`args[0]` to the existing bodies, which move beside it as
`cmd/llmops/{mirror,serve,validate,bench}.go` in package `main`. Their
tests move with them unchanged, which is the point: a refactor that
needed its tests rewritten would not be a refactor.

**Flags do not change.** `--manifest`, `--dir`, `--bucket`, `--url`,
`--cache-root` and the rest keep their names and defaults. Only the
program name and the route to a subcommand move, so every documented
invocation survives a mechanical edit.

The five weight verbs currently parse their flags inside a `mirror`
dispatch that also owns the usage line and the `<repo>@<sha>` splitting.
Flattening lifts that shared preamble to the top-level dispatch rather
than duplicating it five times — `splitRepo` and `popPositional` are
already free functions, so they move up unchanged.

**`llmops version` is new.** It prints the build version and the commit.
A binary that is copied onto hosts by hand needs a way to answer which
one is there; the container mode never needed it because the image tag
answered it.

### What the rename touches outside `cmd/`

| Place | From | To |
|---|---|---|
| Dockerfiles | `ENTRYPOINT ["runtime", "serve"]` | `ENTRYPOINT ["llmops", "serve"]` |
| systemd unit ([[020-bare-metal-packaging]]) | `/usr/local/bin/runtime serve` | `/usr/local/bin/llmops serve` |
| Makefile | `go run ./cmd/runtime validate` | `go run ./cmd/llmops validate` |
| `make dist` | three binaries per platform | one |
| DEPLOY.md, README | three tools | one, with subcommands |

## Acceptance criteria

- **AC1** All nine subcommands work flat, with every flag keeping the
  name and default it has today.
- **AC2** The tests that covered the three binaries pass unchanged in
  their new location, save for the program name and the dropped `mirror`
  word in usage assertions. A test that needed rewriting means behaviour
  moved, which this spec does not permit.
- **AC3** `llmops` with no arguments, and with an unknown subcommand,
  exits non-zero and lists the available subcommands.
- **AC3a** `llmops mirror <anything>` fails as an unknown subcommand
  rather than silently doing nothing, so a stale invocation from a
  script or a doc is loud.
- **AC4** `make dist` produces exactly one binary per platform, and it is
  smaller than the three it replaces. The number goes in this spec.
- **AC5** Every Dockerfile entrypoint, the Makefile, DEPLOY.md, README
  and [[020-bare-metal-packaging]]'s unit file name `llmops`; no
  reference to a `runtime`, `mirror` or `bench` binary survives.
- **AC6** `llmops version` reports a version and commit injected at build
  time, and `make dist` sets them.
- **AC7** `deploycheck` asserts a bare-metal unit's `ExecStart` names the
  `llmops` binary, so a stale unit referencing the old name fails CI
  rather than the host.

## Out of scope

- Renaming or restructuring flags. A rename plus a flag change is two
  migrations wearing one commit.
- Behaviour changes to any subcommand.
- A configuration file, shell completion, or an interactive mode.
- Splitting command bodies into `internal/cli`. They are small and
  already tested where they sit; moving them is churn without a reason.
