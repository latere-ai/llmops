#!/usr/bin/env bash
# Local full-stack e2e (specs/011): real model, real S3 protocol (MinIO),
# real local engine (mlx_lm) — zero cloud cost. Run via `make e2e-local`.
set -euo pipefail
cd "$(dirname "$0")/../.."

PORT=18000
ENGINE_PORT=18001
MINIO_PORT=19000
SHA=c1899de289a04d12100db370d81485cdf75e47ca
REPO=Qwen/Qwen3-0.6B
SCRATCH="${OPENLLMS_E2E_SCRATCH:-$HOME/.cache/openllms-e2e}"
VENV=e2e/local/.venv
BIN=e2e/local/bin
LOG="$SCRATCH/serve.log"
SERVE_PID=""

say() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# docker or podman, whichever exists (macOS setups often alias one to
# the other, and aliases don't reach scripts).
DOCKER=$(command -v docker || command -v podman) || true
cleanup() {
  [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
  [ -n "$DOCKER" ] && "$DOCKER" rm -f openllms-minio >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "bootstrap: venv (mlx-lm, hf) and s5cmd"
[ -n "$DOCKER" ] || fail "docker or podman required"
command -v uv >/dev/null || fail "uv required"
if [ ! -x "$VENV/bin/python" ]; then uv venv "$VENV" --python 3.12; fi
uv pip install --quiet --python "$VENV/bin/python" mlx-lm "huggingface_hub[cli,hf_transfer]"
mkdir -p "$BIN" "$SCRATCH"
if [ ! -x "$BIN/s5cmd" ]; then
  arch=$(uname -m | sed 's/arm64/arm64/;s/x86_64/64bit/')
  curl -fsSL "https://github.com/peak/s5cmd/releases/download/v2.3.0/s5cmd_2.3.0_macOS-${arch}.tar.gz" \
    | tar -xz -C "$BIN" s5cmd
fi
export PATH="$PWD/$BIN:$PWD/$VENV/bin:$PATH"

say "build: runtime/mirror/bench binaries"
go build -o "$BIN/" ./cmd/runtime ./cmd/mirror ./cmd/bench
# Defensive: reap orphans from interrupted prior runs holding our ports.
lsof -ti ":$PORT" -ti ":$ENGINE_PORT" 2>/dev/null | xargs kill 2>/dev/null || true

say "minio: local S3 on :$MINIO_PORT"
"$DOCKER" rm -f openllms-minio >/dev/null 2>&1 || true
# /data lives on the host, not the container VM's disk — a 1.5 GB
# model plus multipart temp files can exhaust a nearly-full VM.
mkdir -p "$SCRATCH/minio"
"$DOCKER" run -d --name openllms-minio -p "$MINIO_PORT:9000" \
  -v "$SCRATCH/minio:/data" \
  quay.io/minio/minio server /data >/dev/null
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
export AWS_REGION=us-east-1 S3_ENDPOINT_URL="http://127.0.0.1:$MINIO_PORT"
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$MINIO_PORT/minio/health/live" >/dev/null 2>&1 && break
  sleep 1
done
s5cmd mb "s3://latere-models" >/dev/null 2>&1 || true

say "mirror: pull $REPO@$SHA (idempotent; ~1.5 GB on first run)"
"$BIN"/mirror pull "$REPO@$SHA" --dir "$SCRATCH/model"

say "mirror: push to MinIO, verify, ls"
"$BIN"/mirror push "$REPO@$SHA" --dir "$SCRATCH/model" --bucket s3://latere-models
"$BIN"/mirror verify "s3://latere-models/$REPO/$SHA/"
"$BIN"/mirror ls --bucket s3://latere-models | grep -q "$REPO/$SHA" || fail "mirror ls missing revision"

say "runtime: validate manifest"
"$BIN"/runtime validate e2e/local/qwen3-0.6b.yaml

serve() {
  OPENLLMS_ENGINE_CMD="$VENV/bin/python -m mlx_lm server --model {model} --host 127.0.0.1 --port {port} --chat-template-args {\"enable_thinking\":false}" \
    "$BIN"/runtime serve --manifest e2e/local/qwen3-0.6b.yaml \
    --port "$PORT" --engine-port "$ENGINE_PORT" --cache-root "$SCRATCH/cache" \
    >>"$LOG" 2>&1 &
  SERVE_PID=$!
}
wait_ready() {
  for _ in $(seq 1 120); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/ready")" = 200 ] && return 0
    kill -0 "$SERVE_PID" 2>/dev/null || { tail -20 "$LOG"; fail "serve exited early"; }
    sleep 2
  done
  tail -20 "$LOG"; fail "/ready never turned 200"
}

say "runtime: serve (cold start; weights staged from MinIO)"
rm -rf "$SCRATCH/cache" # deterministic cold start; HF download cache stays
: >"$LOG"
serve
wait_ready
grep -q "weights: fetching" "$LOG" || fail "cold start should fetch from store"

say "assert: OpenAI chat completion"
RESP=$(curl -fsS "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"default_model","max_tokens":64,"messages":[{"role":"user","content":"Say hello."}]}')
echo "$RESP" | grep -q '"content"' || fail "no content in chat response: $RESP"

say "assert: OpenAI streaming"
SSE=$(curl -fsS -N --max-time 120 "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"default_model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Count to three."}]}')
echo "$SSE" | grep -q "^data:" || fail "no SSE stream: $SSE"

say "assert: Anthropic Messages surface"
ARESP=$(curl -fsS "http://127.0.0.1:$PORT/anthropic/v1/messages" \
  -H 'Content-Type: application/json' \
  -d '{"model":"default_model","max_tokens":64,"messages":[{"role":"user","content":"Say hello."}]}')
echo "$ARESP" | grep -q '"type":"message"' || fail "not an anthropic message: $ARESP"

say "assert: metrics gauge"
curl -fsS "http://127.0.0.1:$PORT/metrics" | grep -q "openllms_weights_load_seconds" || fail "gauge missing"

say "bench: small load"
"$BIN"/bench --url "http://127.0.0.1:$PORT" --model default_model \
  --requests 4 --concurrency 2 --max-tokens 32 --out "$SCRATCH/report.json"
grep -q '"errors": 0' "$SCRATCH/report.json" || fail "bench reported errors: $(cat "$SCRATCH/report.json")"

say "warm start: restart serve, no store fetches"
kill "$SERVE_PID"; wait "$SERVE_PID" 2>/dev/null || true
: >"$LOG"
serve
wait_ready
grep -q "weights: fetching" "$LOG" && fail "warm start re-fetched from store"

say "PASS: full local e2e green (mirror -> MinIO -> runtime -> engine -> dialects -> bench)"
