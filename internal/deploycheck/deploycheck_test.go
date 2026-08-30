package deploycheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/llmops/internal/install"
	"github.com/latere-ai/llmops/internal/manifest"
)

// TestRepoConsistency is the real CI gate: every checked-in model
// manifest must have a consistent deploy manifest (specs/008 AC1).
func TestRepoConsistency(t *testing.T) {
	if err := Validate("../../models", "../../deploy"); err != nil {
		t.Fatal(err)
	}
}

// TestCheckedInUnitsAreGenerated keeps the repo's bare-metal units
// byte-identical to what `llmops install` writes with default options
// (specs/020). Validate() checks a unit's *properties*, which leaves
// room for a checked-in file to drift in ways that still pass; this
// closes that, and means the file in the tree is a truthful preview of
// what lands on a host.
//
// An operator installing with different --cache-root or --user is not
// constrained by this: it pins the repo's copy, not every rendering.
func TestCheckedInUnitsAreGenerated(t *testing.T) {
	models, err := manifest.LoadDir("../../models")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, m := range models {
		if m.DeployMode() != manifest.DeployBareMetal {
			continue
		}
		checked++
		path := filepath.Join("../../deploy", m.Name, m.Name+".service")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", m.Name, err)
			continue
		}
		if want := install.Unit(m, install.Options{}); string(got) != want {
			t.Errorf("%s is not what `llmops install` generates; regenerate it with\n"+
				"  llmops install --manifest models/%s.yaml --print > %s\ngot:\n%s\nwant:\n%s",
				path, m.Name, path, got, want)
		}
	}
	t.Logf("%d bare-metal units checked", checked)
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lws.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeBareMetalModel writes a gb10 manifest in the bare-metal mode,
// carrying the flags specs/019 requires on that class.
func writeBareMetalModel(t *testing.T, dir, name string) {
	t.Helper()
	data := `name: ` + name + `
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
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeUnit(t *testing.T, deployDir, model, execStart string) {
	t.Helper()
	dir := filepath.Join(deployDir, model)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Unit]\nDescription=llmops " + model + "\n\n[Service]\nExecStart=" + execStart +
		"\nRestart=on-failure\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(filepath.Join(dir, model+".service"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBareMetalUnit(t *testing.T) {
	models, deploy := t.TempDir(), t.TempDir()
	writeBareMetalModel(t, models, "qwen")
	writeUnit(t, deploy, "qwen", "/usr/local/bin/llmops serve --manifest /etc/llmops/qwen.yaml")
	if err := Validate(models, deploy); err != nil {
		t.Fatalf("valid bare-metal unit rejected: %v", err)
	}
	// systemd units are written both ways; neither is a failure.
	writeUnit(t, deploy, "qwen", "/usr/local/bin/llmops serve --manifest=/etc/llmops/qwen.yaml")
	if err := Validate(models, deploy); err != nil {
		t.Fatalf("--manifest= form rejected: %v", err)
	}
}

func TestValidateBareMetalUnitFailures(t *testing.T) {
	cases := []struct {
		name string
		exec string
		want string
	}{
		// The specs/024 rename is exactly this failure: a unit left
		// naming the old binary starts nothing, and does so at boot on
		// the host rather than in CI.
		{"stale binary name", "/usr/local/bin/runtime serve --manifest /etc/llmops/qwen.yaml", `want "llmops"`},
		{"no serve verb", "/usr/local/bin/llmops --manifest /etc/llmops/qwen.yaml", "serve subcommand"},
		{"wrong manifest", "/usr/local/bin/llmops serve --manifest /etc/llmops/other.yaml", `want "qwen.yaml"`},
		{"no manifest flag", "/usr/local/bin/llmops serve --cache-root /x", "no --manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, deploy := t.TempDir(), t.TempDir()
			writeBareMetalModel(t, models, "qwen")
			writeUnit(t, deploy, "qwen", tc.exec)
			err := Validate(models, deploy)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsBothArtifacts(t *testing.T) {
	// Whichever artifact the mode does not select goes unchecked, and an
	// unchecked deploy file is how config drifts from what runs.
	models, deploy := t.TempDir(), t.TempDir()
	writeBareMetalModel(t, models, "qwen")
	writeUnit(t, deploy, "qwen", "/usr/local/bin/llmops serve --manifest /etc/llmops/qwen.yaml")
	writeLWS(t, deploy, "qwen", goodLWS("qwen", "ghcr.io/x/llmops-runtime-vllm:v1", "1"))
	err := Validate(models, deploy)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("model with two deploy artifacts accepted: %v", err)
	}
}

func TestValidateBareMetalMissingUnit(t *testing.T) {
	models, deploy := t.TempDir(), t.TempDir()
	writeBareMetalModel(t, models, "qwen")
	if err := Validate(models, deploy); err == nil {
		t.Fatal("bare-metal model with no unit file accepted")
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
	writeLWS(t, deploy, "tiny", goodLWS("tiny", "ghcr.io/latere-ai/llmops-runtime-sglang:v0.1.0", "8"))
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
		{"wrong name", goodLWS("other", "ghcr.io/latere-ai/llmops-runtime-sglang:v1", "8"), "metadata.name"},
		{"wrong image", goodLWS("tiny", "ghcr.io/latere-ai/llmops-runtime-vllm:v1", "8"), "does not match runtime"},
		{"wrong gpus", goodLWS("tiny", "ghcr.io/latere-ai/llmops-runtime-sglang:v1", "4"), "nvidia.com/gpu"},
		{
			"wrong size",
			strings.Replace(goodLWS("tiny", "ghcr.io/latere-ai/llmops-runtime-sglang:v1", "8"), "size: 1", "size: 2", 1),
			"size",
		},
		{
			"wrong ready path",
			strings.Replace(goodLWS("tiny", "ghcr.io/latere-ai/llmops-runtime-sglang:v1", "8"), "/ready", "/readyz", 1),
			"readinessProbe",
		},
		{
			"wrong live path",
			strings.Replace(goodLWS("tiny", "ghcr.io/latere-ai/llmops-runtime-sglang:v1", "8"), "/healthz", "/live", 1),
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
	const good = "ghcr.io/latere-ai/llmops-runtime-sglang:v0.1.0"

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
			multiNodeLWS("big", good, "ghcr.io/latere-ai/llmops-runtime-vllm:v0.1.0", "8"),
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
			multiNodeLWS("tiny", good, "ghcr.io/latere-ai/llmops-runtime-vllm:v1", "4"), "size: 2", "size: 1", 1))
		if err := Validate(models, deploy); err != nil {
			t.Fatal(err)
		}
	})
}

// writeK3Model writes a manifest carrying Kimi-K3's hf_repo, which pins
// the K3-only engine image (specs/018 AC4).
func writeK3Model(t *testing.T, dir, name string) {
	t.Helper()
	writeModel(t, dir, name, "sglang", "")
	p := filepath.Join(dir, name+".yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.ReplaceAll(string(data), "acme/tiny", "moonshotai/Kimi-K3")
	out = strings.Replace(out, `args: ["--tp-size=8"]`,
		`args: ["--tp-size=8", "--trust-remote-code"]`, 1)
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The two SGLang images each contain the other's name as a substring,
// and they are not interchangeable — K3 needs the CUDA 13 branch build,
// and the shared image must not inherit its r580+ driver floor.
func TestValidateEngineImageIsNotSubstringMatched(t *testing.T) {
	const shared = "ghcr.io/latere-ai/llmops-runtime-sglang:v0.1.0"
	const k3 = "ghcr.io/latere-ai/llmops-runtime-sglang-k3:v0.1.0"

	cases := []struct {
		name    string
		write   func(*testing.T, string, string)
		image   string
		wantErr bool
	}{
		{"k3 model on the k3 image", writeK3Model, k3, false},
		{"k3 model on the shared image", writeK3Model, shared, true},
		{
			"ordinary sglang model on the shared image",
			func(t *testing.T, dir, name string) { writeModel(t, dir, name, "sglang", "") },
			shared, false,
		},
		{
			"ordinary sglang model on the k3 image",
			func(t *testing.T, dir, name string) { writeModel(t, dir, name, "sglang", "") },
			k3, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, deploy := t.TempDir(), t.TempDir()
			tc.write(t, models, "m")
			writeLWS(t, deploy, "m", goodLWS("m", tc.image, "8"))
			err := Validate(models, deploy)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), "does not match runtime") {
					t.Fatalf("error %q does not mention the runtime mismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	// The registry prefix stays the operator's choice.
	t.Run("any registry prefix passes", func(t *testing.T) {
		for _, image := range []string{
			"llmops-runtime-sglang:v1",
			"nexus.example.com:5000/latere/llmops-runtime-sglang:v1",
			"123.dkr.ecr.eu-central-1.amazonaws.com/latere/llmops-runtime-sglang@sha256:abc",
		} {
			models, deploy := t.TempDir(), t.TempDir()
			writeModel(t, models, "m", "sglang", "")
			writeLWS(t, deploy, "m", goodLWS("m", image, "8"))
			if err := Validate(models, deploy); err != nil {
				t.Errorf("image %q rejected: %v", image, err)
			}
		}
	})
}

func TestImageName(t *testing.T) {
	cases := map[string]string{
		"llmops-runtime-sglang":                     "llmops-runtime-sglang",
		"llmops-runtime-sglang:v1":                  "llmops-runtime-sglang",
		"ghcr.io/latere-ai/llmops-runtime-vllm:dev": "llmops-runtime-vllm",
		"nexus.example.com:5000/x/llmops-mirror:v1": "llmops-mirror",
		"repo/name@sha256:deadbeef":                 "name",
		"":                                          "",
	}
	for ref, want := range cases {
		if got := imageName(ref); got != want {
			t.Errorf("imageName(%q) = %q, want %q", ref, got, want)
		}
	}
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
	if err := os.WriteFile(filepath.Join(models, "bad.yaml"), []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
