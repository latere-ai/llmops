package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func writeValid(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "tiny.yaml")
	data := `name: tiny
hf_repo: acme/tiny
revision: ` + sha + `
s3_prefix: s3://latere-models/acme/tiny/` + sha + `/
format: safetensors
license: mit
runtime: sglang
load: nvme-cache
gpu: {type: h200, count: 8, nodes: 1}
context_max: 4096
args: ["--tp-size=8"]
`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeLWSFor writes the deploy artifact a k8s model must own, so a
// models directory can be validated the way the repo's own is.
func writeLWSFor(t *testing.T, deployRoot, model, image string, gpus int) {
	t.Helper()
	dir := filepath.Join(deployRoot, model)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: %s
spec:
  leaderWorkerTemplate:
    size: 1
    leaderTemplate:
      spec:
        containers:
          - name: runtime
            image: %s
            resources:
              limits:
                nvidia.com/gpu: "%d"
            readinessProbe:
              httpGet: {path: /ready, port: 8000}
            livenessProbe:
              httpGet: {path: /healthz, port: 8000}
`, model, image, gpus)
	if err := os.WriteFile(filepath.Join(dir, "lws.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFileAndDir(t *testing.T) {
	root := t.TempDir()
	models := filepath.Join(root, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	p := writeValid(t, models)
	writeLWSFor(t, filepath.Join(root, "deploy"), "tiny", "ghcr.io/x/llmops-runtime-sglang:v1", 8)
	var out, errw strings.Builder

	// A single file is a manifest check only: one manifest says nothing
	// about a directory of deploys.
	if code := run([]string{"validate", p}, &out, &errw); code != 0 {
		t.Fatalf("validate file exit %d: %s", code, errw.String())
	}
	if code := run([]string{"validate", models}, &out, &errw); code != 0 {
		t.Fatalf("validate dir exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "1 manifests valid") {
		t.Fatalf("output: %s", out.String())
	}
	if !strings.Contains(out.String(), "deploy artifacts consistent") {
		t.Fatalf("validate did not run the deploy check: %s", out.String())
	}
}

// TestValidateRunsTheDeployCheck pins the gap this wiring closes:
// deploycheck used to run only from its own test, so a deploy file could
// contradict its manifest and nothing a person could run would say so.
func TestValidateRunsTheDeployCheck(t *testing.T) {
	root := t.TempDir()
	models := filepath.Join(root, "models")
	deploy := filepath.Join(root, "deploy", "qwen")
	for _, d := range []string{models, deploy} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data := `name: qwen
hf_repo: acme/tiny
revision: ` + sha + `
format: bf16
license: mit
runtime: vllm
deploy: bare-metal
load: local
gpu: {type: gb10, count: 1, nodes: 1}
context_max: 4096
args: ["--max-model-len=4096", "--gpu-memory-utilization=0.65"]
`
	if err := os.WriteFile(filepath.Join(models, "qwen.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	unit := func(exec string) {
		if err := os.WriteFile(filepath.Join(deploy, "qwen.service"),
			[]byte("[Service]\nExecStart="+exec+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	unit("/usr/local/bin/llmops serve --manifest /etc/llmops/qwen.yaml")
	var out, errw strings.Builder
	if code := run([]string{"validate", models}, &out, &errw); code != 0 {
		t.Fatalf("consistent pair rejected: exit %d: %s", code, errw.String())
	}

	// A unit naming the pre-rename binary must fail the command a person
	// runs, not only a test somewhere in the tree.
	unit("/usr/local/bin/runtime serve --manifest /etc/llmops/qwen.yaml")
	out.Reset()
	errw.Reset()
	if code := run([]string{"validate", models}, &out, &errw); code == 0 {
		t.Fatal("validate accepted a unit naming the old binary")
	}
}

func TestValidateReportsMissingDeployDir(t *testing.T) {
	// Passing quietly with no deploy directory is how the check went
	// unrun in the first place, so absence is an error, not a skip.
	root := t.TempDir()
	models := filepath.Join(root, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	writeValid(t, models)
	var out, errw strings.Builder
	if code := run([]string{"validate", models}, &out, &errw); code == 0 {
		t.Fatal("validate passed with no deploy directory")
	}
	if !strings.Contains(errw.String(), "deploy directory") {
		t.Fatalf("error did not name the missing directory: %q", errw.String())
	}
}

func TestValidateRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("name: bad\nrevision: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errw strings.Builder
	if code := run([]string{"validate", bad}, &out, &errw); code == 0 {
		t.Fatal("invalid manifest must fail")
	}
	if code := run([]string{"validate", dir}, &out, &errw); code == 0 {
		t.Fatal("dir with invalid manifest must fail")
	}
	if code := run([]string{"validate", filepath.Join(dir, "absent")}, &out, &errw); code == 0 {
		t.Fatal("missing path must fail")
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errw strings.Builder
	for _, args := range [][]string{{}, {"bogus"}, {"validate"}, {"serve"}} {
		if code := run(args, &out, &errw); code == 0 {
			t.Fatalf("args %v must fail", args)
		}
	}
}

func TestServeManifestLoadError(t *testing.T) {
	var out, errw strings.Builder
	code := run([]string{"serve", "--manifest", filepath.Join(t.TempDir(), "absent.yaml")}, &out, &errw)
	if code == 0 {
		t.Fatal("serve with missing manifest must fail")
	}
}

// TestServeEndToEnd exercises the CLI serve path with a fake engine
// command; the store prefix is unreachable so Serve fails in weight
// prep — after the flag/env wiring we want covered.
func TestServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	p := writeValid(t, dir)
	t.Setenv("LLMOPS_ENGINE_CMD", "sleep 60")
	var out, errw strings.Builder
	code := run([]string{"serve", "--manifest", p, "--port", "0", "--cache-root", t.TempDir()}, &out, &errw)
	if code == 0 {
		t.Fatal("serve against unreachable store must fail")
	}
	if !strings.Contains(errw.String(), "prepare weights") {
		t.Fatalf("expected prep failure, got: %s", errw.String())
	}
}
