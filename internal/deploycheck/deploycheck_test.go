package deploycheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoConsistency is the real CI gate: every checked-in model
// manifest must have a consistent deploy manifest (specs/008 AC1).
func TestRepoConsistency(t *testing.T) {
	if err := Validate("../../models", "../../deploy"); err != nil {
		t.Fatal(err)
	}
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func writeModel(t *testing.T, dir, name, runtime, extra string) {
	t.Helper()
	image := ""
	args := `args: ["--tp-size=8"]`
	if runtime == "custom" {
		image = "image: ghcr.io/latere-ai/custom:v1\n"
		args = ""
	}
	data := `name: ` + name + `
hf_repo: acme/tiny
revision: ` + sha + `
s3_prefix: s3://latere-models/acme/tiny/` + sha + `/
format: safetensors
license: mit
runtime: ` + runtime + `
` + image + `load: nvme-cache
gpu: {type: h200, count: 8, nodes: 1}
context_max: 4096
` + args + extra
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLWS(t *testing.T, deployDir, model, body string) {
	t.Helper()
	dir := filepath.Join(deployDir, model)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "lws.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goodLWS(name, image, gpus string) string {
	return `apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: ` + name + `
spec:
  leaderWorkerTemplate:
    size: 1
    leaderTemplate:
      spec:
        containers:
          - name: runtime
            image: ` + image + `
            resources:
              limits:
                nvidia.com/gpu: "` + gpus + `"
            readinessProbe:
              httpGet: {path: /ready, port: 8000}
            livenessProbe:
              httpGet: {path: /healthz, port: 8000}
`
}

func TestValidateHappyAndCustom(t *testing.T) {
	models, deploy := t.TempDir(), t.TempDir()
	writeModel(t, models, "tiny", "sglang", "")
	writeLWS(t, deploy, "tiny", goodLWS("tiny", "ghcr.io/latere-ai/open-llms-runtime-sglang:v0.1.0", "8"))
	writeModel(t, models, "ocr", "custom", "")
	writeLWS(t, deploy, "ocr", goodLWS("ocr", "ghcr.io/latere-ai/custom:v1", "8"))
	if err := Validate(models, deploy); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFailures(t *testing.T) {
	cases := []struct {
		name string
		lws  string
		want string
	}{
		{"missing file", "", "lws.yaml"},
		{"empty yaml", "\n", "empty yaml"},
		{"bad yaml", ":\n:", ""},
		{"no lws doc", "kind: Service\n", "no LeaderWorkerSet"},
		{"wrong name", goodLWS("other", "ghcr.io/latere-ai/open-llms-runtime-sglang:v1", "8"), "metadata.name"},
		{"wrong image", goodLWS("tiny", "ghcr.io/latere-ai/open-llms-runtime-vllm:v1", "8"), "does not match runtime"},
		{"wrong gpus", goodLWS("tiny", "ghcr.io/latere-ai/open-llms-runtime-sglang:v1", "4"), "nvidia.com/gpu"},
		{
			"wrong size",
			strings.Replace(goodLWS("tiny", "ghcr.io/latere-ai/open-llms-runtime-sglang:v1", "8"), "size: 1", "size: 2", 1),
			"size",
		},
		{
			"wrong ready path",
			strings.Replace(goodLWS("tiny", "ghcr.io/latere-ai/open-llms-runtime-sglang:v1", "8"), "/ready", "/readyz", 1),
			"readinessProbe",
		},
		{
			"wrong live path",
			strings.Replace(goodLWS("tiny", "ghcr.io/latere-ai/open-llms-runtime-sglang:v1", "8"), "/healthz", "/live", 1),
			"livenessProbe",
		},
		{
			"no containers",
			"kind: LeaderWorkerSet\nmetadata: {name: tiny}\nspec: {leaderWorkerTemplate: {size: 1}}\n",
			"no containers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, deploy := t.TempDir(), t.TempDir()
			writeModel(t, models, "tiny", "sglang", "")
			if tc.lws != "" {
				writeLWS(t, deploy, "tiny", tc.lws)
			}
			err := Validate(models, deploy)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// writeMultiNodeModel writes a manifest with gpu.nodes = 2 — the shape
// specs/018 keeps in reserve as Kimi-K3's H200 fallback.
func writeMultiNodeModel(t *testing.T, dir, name string) {
	t.Helper()
	writeModel(t, dir, name, "sglang", "")
	p := filepath.Join(dir, name+".yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(data), "nodes: 1", "nodes: 2", 1)
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// multiNodeLWS is a 2-node group; worker holds the same image and GPU
// count as the leader but serves no HTTP, so it carries no probes.
func multiNodeLWS(name, leaderImage, workerImage, workerGPUs string) string {
	return `apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: ` + name + `
spec:
  leaderWorkerTemplate:
    size: 2
    leaderTemplate:
      spec:
        containers:
          - name: runtime
            image: ` + leaderImage + `
            resources:
              limits:
                nvidia.com/gpu: "8"
            readinessProbe:
              httpGet: {path: /ready, port: 8000}
            livenessProbe:
              httpGet: {path: /healthz, port: 8000}
    workerTemplate:
      spec:
        containers:
          - name: runtime
            image: ` + workerImage + `
            resources:
              limits:
                nvidia.com/gpu: "` + workerGPUs + `"
`
}

func TestValidateMultiNode(t *testing.T) {
	const good = "ghcr.io/latere-ai/open-llms-runtime-sglang:v0.1.0"

	t.Run("consistent worker passes", func(t *testing.T) {
		models, deploy := t.TempDir(), t.TempDir()
		writeMultiNodeModel(t, models, "big")
		writeLWS(t, deploy, "big", multiNodeLWS("big", good, good, "8"))
		if err := Validate(models, deploy); err != nil {
			t.Fatal(err)
		}
	})

	cases := []struct {
		name string
		lws  string
		want string
	}{
		{
			"worker missing entirely",
			// Group size says 2 nodes, but only the leader is defined:
			// the second rank never starts.
			strings.Replace(goodLWS("big", good, "8"), "size: 1", "size: 2", 1),
			"requires a workerTemplate",
		},
		{
			"worker on the wrong engine image",
			multiNodeLWS("big", good, "ghcr.io/latere-ai/open-llms-runtime-vllm:v0.1.0", "8"),
			"workerTemplate image",
		},
		{
			"worker short on GPUs",
			multiNodeLWS("big", good, good, "4"),
			"workerTemplate nvidia.com/gpu",
		},
		{
			"worker has no containers",
			strings.Replace(multiNodeLWS("big", good, good, "8"),
				"    workerTemplate:\n      spec:\n        containers:", "    workerTemplate:\n      spec:\n        xcontainers:", 1),
			"no containers in workerTemplate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, deploy := t.TempDir(), t.TempDir()
			writeMultiNodeModel(t, models, "big")
			writeLWS(t, deploy, "big", tc.lws)
			err := Validate(models, deploy)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// A single-node model is never asked for a worker, and a stray one
	// is not the validator's business.
	t.Run("single node ignores workerTemplate", func(t *testing.T) {
		models, deploy := t.TempDir(), t.TempDir()
		writeModel(t, models, "tiny", "sglang", "")
		writeLWS(t, deploy, "tiny", strings.Replace(
			multiNodeLWS("tiny", good, "ghcr.io/latere-ai/open-llms-runtime-vllm:v1", "4"), "size: 2", "size: 1", 1))
		if err := Validate(models, deploy); err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidateCustomImageMismatch(t *testing.T) {
	models, deploy := t.TempDir(), t.TempDir()
	writeModel(t, models, "ocr", "custom", "")
	writeLWS(t, deploy, "ocr", goodLWS("ocr", "ghcr.io/latere-ai/wrong:v1", "8"))
	if err := Validate(models, deploy); err == nil || !strings.Contains(err.Error(), "manifest image") {
		t.Fatalf("custom image mismatch not caught: %v", err)
	}
}

func TestValidateEmptyModels(t *testing.T) {
	if err := Validate(t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("empty models dir must error")
	}
}

func TestValidateBadModelsDir(t *testing.T) {
	models := t.TempDir()
	os.WriteFile(filepath.Join(models, "bad.yaml"), []byte("name: x\n"), 0o644)
	if err := Validate(models, t.TempDir()); err == nil {
		t.Fatal("invalid model manifest must error")
	}
}

func TestDNSName(t *testing.T) {
	if DNSName("kimi-k2.7-code") != "kimi-k2-7-code" {
		t.Fatal("dots must map to dashes")
	}
}

func TestDig(t *testing.T) {
	v := map[string]any{"a": map[string]any{"b": 1}}
	if dig(v, "a", "b") != 1 || dig(v, "a", "c") != nil || dig(v, "x", "y") != nil || dig(1, "a") != nil {
		t.Fatal("dig misbehaves")
	}
}
