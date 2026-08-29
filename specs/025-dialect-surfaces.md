---
title: "Every caller dialect, and an engine dialect that is declared"
status: complete
depends_on:
  - 003-serving-runtime.md
  - 009-lux-integration.md
affects:
  - internal/runtime/shim.go
  - internal/manifest/
  - README.md
  - docs/deploy.md
effort: medium
created: 2026-08-29
updated: 2026-08-29
author: changkun
dispatched_task_id: null
---

# Every caller dialect, and an engine dialect that is declared

## Where we are

The shim serves one translated surface, wired at construction:

```go
translator: &llmdialect.Translator{
    Frontend: anthropic.NewFrontend(),
    Backend:  openaichat.NewBackend(openaichat.BackendOptions{}),
}
```

One frontend, one backend, one hardcoded path. Everything else is
reverse-proxied to the engine, which means the served surface is exactly
"whatever the engine speaks, plus Anthropic Messages".

That is narrower than what we already depend on.
`latere.ai/x/pkg/llmdialect` ships **four** dialects — Anthropic
Messages, OpenAI Chat Completions, OpenAI Responses, and the lux-native
dialect — each with both a `Frontend` (caller side) and a `Backend`
(upstream side), translating hub-and-spoke through a neutral IR. We use
two of the eight codecs.

## Two things are hardcoded, and they are different problems

### 1. The caller side is one dialect out of three we could serve

`openairesp.NewFrontend()` exists and is unused. A caller holding an
OpenAI Responses client cannot talk to our endpoints at all, though the
translation is already written and tested upstream.

**Decision: serve a table of caller dialects, not a special case.**

| Path | Dialect |
|---|---|
| `/v1/chat/completions` | OpenAI Chat Completions |
| `/v1/messages` | Anthropic Messages |
| `/v1/responses` | OpenAI Responses |

`/v1/messages` rather than today's `/anthropic/v1/messages`: it is the
path an unmodified Anthropic SDK requests, so pointing one at the
endpoint's base URL just works. Nothing is deployed, so the move costs
nothing now and would cost a coordinated change later.

**It did cost something, and the cost is worth recording.**
`e2e/local/run.sh` ([[011-local-e2e]]) called the old path in two
places. The docs moved with the route and the script did not, so the
local e2e's Anthropic assertions had been curling a dead path since this
landed — found 2026-08-29. "Nothing is deployed" counted deployments and
missed a caller: a shell script is a caller like any other, and it is
the kind no compiler checks. Fixed, with
`TestE2EScriptCallsOnlyServedPaths` requiring every shim path the script
curls to be one the route table answers.

**Check before landing:** that the engine does not itself serve
`/v1/messages`, or the proxy default and this route collide. vLLM
v0.28.0 does not, but this is a per-engine-version fact, not a law.

**The lux dialect stays out.** That surface belongs to the gateway,
which embeds the same package ([[009-lux-integration]]). Serving it here
would put the same dialect in two places with two owners.

### 2. The engine side is an assumption, not a fact

`Backend: openaichat.NewBackend(...)` is true of SGLang and vLLM, and
the code states it nowhere. It is a property of the *engine*, and the
manifest already carries the engine.

**Decision: `engine_dialect` on the manifest, defaulting to
`openai-chat`.** Every existing manifest keeps its meaning. An engine
that speaks something else declares it, and the shim builds each
translator against that backend rather than a compiled-in one.

This is not hypothetical. `latere.ai/tgo` serves OpenAI Chat, Anthropic
Messages, and OpenAI Responses natively; pointing our shim at it while
assuming OpenAI Chat would translate Anthropic → OpenAI Chat only for
the engine to translate back, losing on both hops for no reason.

### Passthrough when the dialects match

When a caller's dialect equals the engine's, there is nothing to
translate: the request is proxied, with system-prompt injection applied
if the manifest asks for it. That is what `/v1/chat/completions` does
today, and it generalises — it should fall out of the table, not be a
separate branch.

## The loss report is collected and then dropped

This is the part worth fixing regardless of the rest.

`llmdialect` deliberately does **not** silently drop fields the target
dialect cannot represent. It collects them in `ir.Request.Loss`, and the
package documentation calls this out: logprobs asked for through
Anthropic Messages, which has no member for them, is reported as loss
rather than ignored.

The shim decodes the request, binds the result to `req`, uses
`req.Stream`, and never reads `req.Loss`. So the caller asks for
something, the package says "this was lost", and we throw the report
away — which produces exactly the silent drop the package was written to
prevent.

**Decision: surface it in both directions.**

- To the caller, a response header naming the dropped fields. A client
  can then tell "the model did not return logprobs" from "logprobs are
  not representable on this surface", which are different bugs.
- To the operator, a Prometheus counter keyed by field and dialect pair.
  How often callers ask for something a surface cannot carry is a fact
  about whether the surface is the right one.

## Acceptance criteria

- **AC1** All three caller dialects are served, each answering a real
  request against a live engine: non-streaming and streaming.
- **AC2** `engine_dialect` validates, defaults to `openai-chat`, and
  every existing manifest passes unchanged.
- **AC3** A caller dialect equal to the engine dialect is proxied, not
  round-tripped through the IR. A test asserts the body reaching the
  engine is byte-identical to what the caller sent, save for a system
  prompt the manifest asked to inject.
- **AC4** `ir.Request.Loss` is non-empty for at least one real
  request/surface pair in a test, and that loss appears in the response
  header and increments the counter. This is the criterion the current
  code silently fails.
- **AC5** A dialect the engine cannot serve is rejected at manifest
  validation, not at request time.
- **AC6** The Anthropic surface moves to `/v1/messages`, and a test
  asserts the shim's route table and the engine's own routes do not
  overlap for the pinned engine version.
- **AC7** README and docs/deploy.md list the served surfaces per model, since
  "what can talk to this endpoint" is now a manifest-level answer.

## Out of scope

- Serving the lux dialect ([[009-lux-integration]]).
- Embeddings, audio, or image-generation surfaces. This spec is about
  chat-shaped dialects, which is what `llmdialect` covers.
- Translating *between* two non-native dialects in one hop for its own
  sake; hub-and-spoke through the IR is the package's design and this
  spec does not second-guess it.
- Per-caller dialect negotiation by header. The path names the dialect,
  which is what every client already expects.
