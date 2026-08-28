# Deployment Guide

How to take a model from Hugging Face to a serving endpoint behind Lux:
build the images, freeze the weights into S3, deploy on GPU Kubernetes,
and verify. The configuration reference at the end lists every
customization knob. Design rationale lives in [`specs/`](./specs/README.md).

## Prerequisites

| What | Why | Notes |
|---|---|---|
| An S3-compatible bucket | frozen weights home | AWS S3, DO Spaces, R2, MinIO, anything s5cmd speaks. Enable versioning; Object Lock if supported. The checked-in manifests point at a bucket named `latere-models`; change `s3_prefix` in `models/*.yaml` to use your own. |
| k8s Secret `mirror-s3` in ns `llmops` | mirror Job + node cache credentials | keys: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, plus `S3_ENDPOINT_URL` for non-AWS. Optional `HF_TOKEN` for gated repos. |
| GPU nodes + NVIDIA GPU Operator | run the engines | node pools labeled `latere.ai/gpu-pool: h200`, `b200`, or `b300`; NVMe at `/var/cache/llmops`. The `b300` pool needs an **r580+ driver** — Kimi-K3's image is CUDA 13 only |
| [LeaderWorkerSet](https://github.com/kubernetes-sigs/lws) installed | pod-group primitive for (multi-node-ready) serving | `kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/latest/download/manifests.yaml` |
| `docker login <registry>` | push images | default registry is `ghcr.io/latere-ai`; any OCI registry works (ECR, Nexus, Harbor, …) via `REGISTRY=` |

## 1. Build and push images

Four images, versioned together:

```sh
make release VERSION=v0.1.0
# or into your own registry (ECR, Nexus, Harbor, ...):
make release VERSION=v0.1.0 REGISTRY=nexus.example.com/latere
```

builds and pushes `linux/amd64` images (default registry
`ghcr.io/latere-ai`; the image *names* are fixed, the registry prefix is
yours):

- `llmops-runtime-sglang` — SGLang engine (pinned; see the [engine decision record](./specs/001-inference-engine-selection.md)) + `runtime` entrypoint
- `llmops-runtime-sglang-k3` — Kimi-K3-capable SGLang (CUDA 13, r580+ driver). Separate image because that driver requirement should not reach the h200/b200 pools
- `llmops-runtime-vllm` — vLLM engine + `runtime` entrypoint (also the `load: s3-stream` path)
- `llmops-mirror` — `mirror` CLI + `hf` + `s5cmd`, for the weight-freeze Job

Engine versions are pinned in the Dockerfiles — bump them deliberately,
never `latest`. After a release, update the image references in
`deploy/*/lws.yaml` (and `deploy/mirror/job.yaml`) — the consistency
check validates the image *name* against the manifest's runtime, not the
registry, so a custom registry passes CI unchanged. If your registry is
private, add an `imagePullSecrets` entry to the pod specs.

## 2. Ship a model (freeze weights into S3)

One-time per model revision. In-cluster (recommended — bandwidth and
disk live there):

```sh
# Edit deploy/mirror/job.yaml: set metadata.name, MODEL_REPO, MODEL_SHA,
# --bucket, and the scratch volume size (>= the model's size on disk).
kubectl -n llmops apply -f deploy/mirror/job.yaml
kubectl -n llmops logs -f job/mirror-<name>
```

The Job pulls from HF (SHA256-verified against LFS OIDs, safetensors
only), uploads via s5cmd, and writes `_manifest.json` last — its
presence marks the mirror complete. Re-running is idempotent; verify
anytime:

```sh
llmops verify s3://<your-bucket>/<org>/<repo>/<sha>/
llmops list --bucket s3://<your-bucket>
```

Then pin the model in `models/<name>.yaml` (see the configuration
reference below) and run `make validate`. CI enforces that every model
manifest has a consistent `deploy/<name>/lws.yaml`.

## 3. Deploy and serve

```sh
kubectl create namespace llmops --dry-run=client -o yaml | kubectl apply -f -

# The runtime reads the manifest from a ConfigMap:
kubectl -n llmops create configmap kimi-k2-7-code-manifest \
  --from-file=model.yaml=models/kimi-k2.7-code.yaml

kubectl -n llmops apply -f deploy/kimi-k2.7-code/lws.yaml
```

Watch startup — the pod stages weights from S3 onto node NVMe, then
launches the engine:

```sh
kubectl -n llmops get pods -w
kubectl -n llmops logs -f <pod>   # "weights: fetching ..." then "launching sglang"
```

`/ready` returns 503 during load and 200 when the engine is up
(readiness probe allows a long cold start; warm restarts on the same
node skip the download entirely). Verify the endpoint:

```sh
kubectl -n llmops port-forward svc/kimi-k2-7-code 8000 &

# OpenAI surface (native engine passthrough)
curl -s localhost:8000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"kimi-k2.7-code","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'

# Anthropic surface (llmdialect translation)
curl -s localhost:8000/anthropic/v1/messages -H 'Content-Type: application/json' \
  -d '{"model":"kimi-k2.7-code","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'

# Metrics (engine passthrough + llmops_weights_load_seconds)
curl -s localhost:8000/metrics | grep llmops

# Baseline benchmark (produces the numbers the gateway's cost config needs)
llmops bench --url http://localhost:8000 --model kimi-k2.7-code \
  --concurrency 8 --requests 32 --out report.json
```

Finally register the in-cluster endpoint
(`http://<name>.llmops.svc:8000/v1`) as a provider in Lux. Lux is the
only ingress; engine pods are never exposed publicly.

**License gates:** check `license`/`license_note` in the model manifest
before you expose it through the gateway. MiniMax-M3 requires a one-time
commercial notice, which must be sent first. Kimi-K3's
Model-as-a-Service clause turns on whether gateway exposure counts as
internal use, and no such determination has been recorded, so K3 must not
be exposed yet. Kimi-K2.7 carries a modified-MIT attribution clause.
DeepSeek's V4 checkpoints are plain MIT, with no gate.

## Local rehearsal

The full pipeline runs on a laptop at zero cloud cost — same manifests,
same tools, MinIO for S3, a 0.6B model, mlx as the engine:

```sh
make e2e-local
```

Use it to validate changes to the runtime and mirror before touching real
hardware. It needs Docker or Podman, `uv`, and Apple silicon, since the
local engine is mlx.

## Configuration reference

### Model manifest (`models/<name>.yaml`) — the deploy's source of truth

| Field | Values | Effect |
|---|---|---|
| `name` | `[a-z0-9.-]+` | model id callers use; k8s resources use it with `.`→`-` |
| `hf_repo`, `revision` | repo + 40-hex commit SHA | pinned identity; `revision` must match the S3 prefix |
| `s3_prefix` | `s3://<bucket>/<hf_repo>/<revision>/` | where frozen weights live (validated shape) |
| `format` | free text (`fp8`, `int4-qat`, …) | documentation of the checkpoint format |
| `license`, `license_note` | free text | compliance record; gates noted here block Lux exposure |
| `runtime` | `sglang` \| `vllm` \| `custom` | which engine image; `custom` requires `image:` and serves any container honoring the health contract |
| `image` | image ref | custom-runtime container (OCR wrappers etc.) |
| `load` | `nvme-cache` (default) \| `s3-stream` | staged via node NVMe, or vLLM-only direct S3 streaming |
| `gpu` | `{type, count, nodes}` | resource shape; must match the LWS manifest (CI-checked) |
| `context_max` | int | documented context config; pair with the KV-cache args it needs |
| `args` | list, verbatim | engine CLI flags — parallelism (`--tp-size`), parsers (`--tool-call-parser`), quantization, KV dtype. Per-model required flags are enforced (MiniMax `--block-size=128`; Kimi-K3 and V4-Flash-0731 `--trust-remote-code`), as are the DSpark constraints: with `--speculative-algorithm DSPARK`, a separate `--speculative-draft-model-path`, `--pp-size` > 1, or DP attention are rejected |
| `system_prompt` | `{mode, text}` | enforced by the shim on every request, both dialects: `default` (only when caller sends none) \| `prepend` \| `override` |

The runtime always adds `--served-model-name <name>` so callers address
the manifest name, and renders the base engine command itself — `args`
only carries model-specific flags.

### Runtime container (flags / env)

| Knob | Default | Purpose |
|---|---|---|
| `--manifest` | `/etc/llmops/model.yaml` | manifest path (mounted ConfigMap) |
| `--port` | 8000 | shim/service port (`/healthz`, `/ready`, `/metrics`, `/v1/*`, `/anthropic/v1/messages`) |
| `--engine-port` | 30000 | engine's internal port |
| `--cache-root` | `/cache` | NVMe cache mount; keyed by repo+revision, flock-shared across pods on a node |
| `LLMOPS_ENGINE_CMD` | unset | replace the engine command (`{model}`/`{port}` substituted) — local/dev substitution, e.g. mlx |
| `LLMOPS_ENGINE_HEALTH_PATH` | `/health` | engine health endpoint, for engines that differ |

### Deploy manifest (`deploy/<name>/lws.yaml`)

| Knob | Where | Notes |
|---|---|---|
| replicas | `spec.replicas` | whole serving groups (capacity planning, not HPA) |
| group size | `leaderWorkerTemplate.size` | = `gpu.nodes`; >1 activates multi-node (needs RoCEv2/NCCL) and requires a `workerTemplate` (CI-checked: same image and GPU count as the leader, no probes — only rank 0 serves HTTP) |
| GPU count/pool | `resources.limits."nvidia.com/gpu"`, `nodeSelector` | must match manifest `gpu` (CI-checked); pool label selects H200 / B200 / B300 |
| image ref | container `image` | `<REGISTRY>/llmops-runtime-<engine>:<VERSION>` from `make release`; registry prefix is free, name must match the manifest runtime (CI-checked) |
| NVMe cache | `volumes.cache.hostPath` | `/var/cache/llmops`; a prefetch DaemonSet warms it |
| `/dev/shm` | `volumes.shm.sizeLimit` | ≥32Gi (vLLM requires it for DeepSeek-V4-class models) |
| probe budget | `readinessProbe.failureThreshold` | cold start for the big models is minutes — size it accordingly |

### Mirror Job (`deploy/mirror/job.yaml`)

| Knob | Purpose |
|---|---|
| `MODEL_REPO`, `MODEL_SHA` | which revision to freeze |
| `mirror-s3` Secret | bucket credentials; `S3_ENDPOINT_URL` for DO Spaces/R2/MinIO |
| scratch volume size | ≥ model size on disk (167 GB to 1.6 TB across the current set; Kimi-K3 alone is 1561 GB) |

## Troubleshooting

- **`/ready` stuck at 503** — check pod logs: still `weights: fetching`
  (normal on cold start), engine crash (log tail shows the engine's
  stderr), or a hash mismatch (store corruption → run `llmops verify`).
- **404 from `/v1/chat/completions`** — model id in the request must be
  the manifest `name` (that's the served model name).
- **mirror Job fails mid-upload** — re-run it; push is idempotent and
  skips verified files. `_manifest.json` absent = mirror incomplete.
- **Second pod on a node re-downloads** — cache is keyed by
  repo+revision under `--cache-root`; confirm the hostPath mount and
  that revisions actually match.
