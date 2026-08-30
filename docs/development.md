# Development

Building, testing, and contributing to llmops itself. To *operate* it,
read the [deploy guide](./deploy.md) instead.

## Build

Go 1.27 or newer, no cgo, no other build dependency:

```sh
git clone https://github.com/latere-ai/llmops.git
cd llmops
make build                       # go build ./...
make dist                        # static linux/amd64 + linux/arm64 binaries
make hooks                       # pre-commit gofmt + modernizer guard
```

`make dist` stamps `version` and `commit` into the binary; a plain
`go build` still answers `llmops version` from what the toolchain
recorded.

## Test

```sh
make test           # go vet + go test ./...
make cover          # ≥90% per package, not a repository average
make test-hermetic  # the suite with only the toolchain on PATH
make test-race      # CGO_ENABLED=1 go test -race ./...
make spec-lint      # the spec tree against .lateregate.yaml
make validate       # models/*.yaml, their deploy artifacts, and `deps`
make e2e            # pull→push→verify and serve→ready→metrics
make e2e-local      # the whole pipeline on a laptop
```

**`make cover` gates each package, not the repository.** An average lets a
well-tested package carry an untested one and reports a number nobody can act
on: this repository once passed at 90.4% while `internal/harness` sat at 85.7%
and `internal/install` at 87.8%, both invisible behind it.

**`make test-hermetic` runs the suite with `PATH` stripped** to the Go
toolchain and the directories `.lateregate.yaml` names. Three CI failures in
one day came from tests that depended on what happened to be installed on the
machine running them — `systemctl` absent on macOS and present-but-unprivileged
on a runner, a harness binary on a laptop's `PATH` and not on a runner's. Each
passed locally and failed in CI, which is the worst order to find out.

Every one of these needs nothing but a Go toolchain and this checkout, and CI
runs all of them on every push. The checks behind them live in
[`latere.ai/x/ci-gate`](https://github.com/latere-ai/ci-gate), pinned as a tool
dependency in `go.mod`, so a gate runs the same here as on a runner — which is
the point, because a gate you can only run in CI tells you too late. What this
repository asserts about itself — the coverage floor and its exemptions, the
spec vocabulary, the dependency allowlist — is in `.lateregate.yaml`.
`make e2e` is a subset of the same suite: it drives the real code paths
against in-process fakes, so it needs no GPU, no network and no
credentials. Nothing in these targets is skipped for missing
configuration.

`make e2e-local` is the only target with external prerequisites: Docker
or Podman, [uv](https://docs.astral.sh/uv/), and a one-time download of
roughly 1.5 GB. It runs the full pipeline against MinIO for S3 and a
0.6B model under mlx, at zero cloud cost. It also needs Apple silicon,
since mlx is the local engine.

GPU serving on real hardware is not covered by any automated suite. It
is a per-model release gate, run by hand against the model's spec.

## Layout

```
cmd/llmops/   the one command (weights, serving, bench)
internal/
  manifest/   the models/*.yaml schema and its validation
  mirror/     Hugging Face fetch, checksum, freeze, S3 push/verify
  runtime/    the serving entrypoint: weight prep, engine launch, shim
  install/    systemd unit rendering for the bare-metal mode
  deploycheck/ manifest ↔ deploy-artifact consistency
  bench/      the load generator behind `llmops bench`
deploy/       one artifact per model: a k8s LeaderWorkerSet, or a unit
models/       per-model manifests
e2e/          fake-backed pipeline tests, plus the local full-stack run
specs/        design records — start at specs/README.md
```

## Conventions

- Every change carries a test that fails without it.
- Specs come before implementation for anything larger than a fix, and
  the spec is updated in the same session the work lands.
- Docs are written for whoever reads them: `README` and `docs/` for
  users of the tool, `specs/` for people changing it, comments for
  whoever debugs the code.

## APIs are not frozen

The manifest schema, the shim's endpoints and the CLI flags may change
while the first models are brought up.
