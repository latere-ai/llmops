---
title: "Pointing a coding harness at a model you own"
status: draft
depends_on:
  - 020-bare-metal-packaging.md
  - 024-single-cli.md
  - 025-dialect-surfaces.md
affects:
  - cmd/llmops/
  - internal/install/
  - README.md
  - PRACTICE.md
effort: medium
created: 2026-08-29
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Pointing a coding harness at a model you own

## The gap

A model is serving on a host. To use it from Claude Code, Codex or
opencode you must know its port, invent an API key because there is no
auth, and write the config in whichever of three formats that harness
happens to want — under whichever of three sets of variable names.

Every one of those is derivable from the manifest. None of it is
automated, so the last mile from "we serve this" to "I am coding against
it" is done by hand, from memory, differently each time.

Three verbs close it.

```
llmops ps                                what is serving on this host
llmops endpoint --harness claude         config to paste or eval
llmops run claude --model qwen3.8-27b    launch the harness against it
```

## `ps`, not `ls`

`llmops list` already exists and lists **mirrored weights** in a store.
A second listing verb one character away, for a different object, is the
ambiguity [[024-single-cli]] renamed `ls` to `list` to avoid — bringing
`ls` back for running processes would undo that on purpose.

`ps` is unambiguous, and means "what is running" in every shell anyone
using this has already used. The two verbs then read as what they are:

```
llmops list --bucket …    weights we have frozen
llmops ps                 models answering requests right now
```

### How it discovers

Host-local and mode-agnostic, in three steps:

1. Read installed manifests from the config directory
   (`/etc/llmops/*.yaml`) — what [[020-bare-metal-packaging]]'s install
   placed there.
2. Take each model's port from its unit's `ExecStart`, defaulting to
   8000. The manifest has no port field, and should not grow one: the
   port is a property of the deployment, and the unit already carries
   it.
3. Probe `/ready` and `/metrics` on that port.

No systemd query, so it works on a host where the process was started by
hand, and it reports what is *answering* rather than what is supposed
to be.

```
NAME          STATE    PORT  RUNTIME  GPU      LOADED
qwen3.8-27b   ready    8000  vllm     1xgb10   39.2s
```

`LOADED` comes from `llmops_weights_load_seconds`, which the shim
already exports. Kubernetes models are out of scope: this answers
"what is on this box", and a cluster has its own answer.

## `endpoint --harness <name>`

**This is a formatting table, not dialect logic.** Once
[[025-dialect-surfaces]] serves all three surfaces on one port, every
harness points at the same address and picks its own path:

```
http://host:8000/v1/chat/completions   Codex, opencode
http://host:8000/v1/messages           Claude Code
http://host:8000/v1/responses          anything speaking Responses
```

So `endpoint` never asks "which dialect does this model serve" — the
answer is always "all of them". What actually differs between harnesses
is smaller than it looks:

| Harness | Consumes | Keys | Base URL |
|---|---|---|---|
| `claude` | shell env | `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL` | `http://host:8000` |
| `codex` | shell env, or `~/.codex/config.toml` | `OPENAI_BASE_URL`, `OPENAI_API_KEY` | `http://host:8000/v1` |
| `opencode` | `opencode.json` | `provider.<id>` with `@ai-sdk/openai-compatible`, `options.baseURL` | `http://host:8000/v1` |

Three columns of difference: the **variable names**, the **config
format**, and whether the base URL carries the `/v1` suffix — an SDK
convention (the Anthropic client appends `/v1/messages` to a bare base,
the OpenAI client appends `/chat/completions` to one ending in `/v1`),
not a property of what we serve.

Default output is whatever that harness reads; `--format env|json|toml`
overrides. `env` output is `eval`-able:

```sh
eval "$(llmops endpoint --harness claude --model qwen3.8-27b)"
```

Adding a fourth harness is a row. If it ever requires more than a row,
the surfaces are wrong, not the table.

### This is why 025 comes first

Until 025 lands, the Anthropic surface is at `/anthropic/v1/messages`
and Claude Code's base URL needs an `/anthropic` suffix that no other
harness wants. Building `endpoint` against that would mean writing
per-model surface derivation, shipping it, and deleting it a week later
when the paths unify.

**026 depends on 025 rather than working around it.** The dependency is
the design: the translator layer is what makes this a table.

### Auth: there is none, and that should be visible

The shim has no authentication. Every harness nonetheless requires a
token to be set, or it falls back to its own login flow.

So `endpoint` emits a placeholder (`local`) **and says on stderr that
the endpoint is unauthenticated**. On a Tailscale-only lab box that is a
defensible choice; it should be a choice someone made, not a thing they
discover. Real auth belongs to Lux ([[009-lux-integration]]), which is
why this spec does not add any.

## `run <harness> --model <name>`

Resolves the model, builds the same environment `endpoint` prints, and
`exec`s the harness — replacing the process, so signals, exit codes and
the terminal behave exactly as running the harness directly.

```sh
llmops run claude --model qwen3.8-27b
llmops run codex  --model qwen3.8-27b -- --full-auto
```

Arguments after `--` pass through untouched.

**It does not start a stopped model.** `run` is a wrapper around a
harness, and a verb that silently starts a ten-minute model load because
you typed a name is doing something you did not ask for. If the model is
not ready, it says so and prints the command that starts it. `--wait`
blocks on an already-starting model, which is the case that is actually
useful given the load times in [[019-gb10-serving-target]].

## Acceptance criteria

- **AC1** `ps` lists a serving model with its state, port, runtime, GPU
  and weight-load time; a model whose unit exists but is not answering
  shows as not ready rather than being omitted.
- **AC2** `ps` works with no systemd present, and reports what answers
  rather than what is configured.
- **AC3** `endpoint` emits, for each of the three harnesses, config that
  a real harness accepts unmodified. Verified by launching each against
  a live endpoint, not by string comparison.
- **AC4** `endpoint --format env` output is `eval`-able and sets exactly
  the variables that harness reads.
- **AC5** The per-harness difference is data, not code: adding a
  harness is a table row, and a test asserts the emitters share one
  code path rather than branching per harness.
- **AC6** `endpoint` warns on stderr that the endpoint is
  unauthenticated, and the warning does not pollute `eval`-able stdout.
- **AC7** `run` execs the harness, passes through arguments after `--`,
  and propagates its exit code. A model that is not ready produces the
  start command rather than a hang or a partial launch.
- **AC8** An unknown harness name lists the known ones.

## Out of scope

- Authentication. Lux owns that ([[009-lux-integration]]).
- Starting and stopping models. `systemctl` does that, and
  [[020-bare-metal-packaging]] generates the unit for it.
- Kubernetes models in `ps`.
- Editing a harness's config files in place. Printing config a person
  can read and paste is reversible; rewriting `~/.codex/config.toml`
  is not.
- Harnesses beyond the three named. The registry is a table; adding a
  fourth should be a row, and if it is not, the design is wrong.
