package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "74797c9c62378b951a1f6fcf5c4631024e9b8bef"

func valid() *Manifest {
	return &Manifest{
		Name:       "kimi-k2.7-code",
		HFRepo:     "moonshotai/Kimi-K2.7-Code",
		Revision:   sha,
		S3Prefix:   "s3://latere-models/moonshotai/Kimi-K2.7-Code/" + sha + "/",
		Format:     "int4-qat",
		License:    "modified-mit",
		Runtime:    RuntimeSGLang,
		Load:       LoadNVMeCache,
		GPU:        GPU{Type: "h200", Count: 8, Nodes: 1},
		ContextMax: 262144,
		Args:       []string{"--tp-size=8"},
	}
}

func TestValidateOK(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Manifest)
		want string
	}{
		{"bad name", func(m *Manifest) { m.Name = "Bad Name" }, "name"},
		{"empty name", func(m *Manifest) { m.Name = "" }, "name"},
		{"bad repo", func(m *Manifest) { m.HFRepo = "nope" }, "hf_repo"},
		{"unpinned revision", func(m *Manifest) { m.Revision = "main" }, "pinned"},
		{"short revision", func(m *Manifest) { m.Revision = "abc123" }, "pinned"},
		{"prefix scheme", func(m *Manifest) { m.S3Prefix = "gs://x/y/" }, "s3://"},
		{"prefix no bucket", func(m *Manifest) { m.S3Prefix = "s3://" }, "bucket"},
		{"prefix mismatch", func(m *Manifest) {
			m.S3Prefix = "s3://latere-models/other/Repo/" + sha + "/"
		}, "hf_repo/revision"},
		{"prefix unpinned", func(m *Manifest) {
			m.S3Prefix = "s3://latere-models/moonshotai/Kimi-K2.7-Code/main/"
		}, "hf_repo/revision"},
		{"no format", func(m *Manifest) { m.Format = "" }, "format"},
		{"no license", func(m *Manifest) { m.License = "" }, "license"},
		{"bad runtime", func(m *Manifest) { m.Runtime = "tgi" }, "runtime"},
		{"engine with image", func(m *Manifest) { m.Image = "ghcr.io/x/y" }, "custom"},
		{"engine no args", func(m *Manifest) { m.Args = nil }, "args"},
		{"custom no image", func(m *Manifest) { m.Runtime = RuntimeCustom }, "image"},
		{"bad load", func(m *Manifest) { m.Load = "disk" }, "load"},
		{"s3-stream on sglang", func(m *Manifest) { m.Load = LoadS3Stream }, "vllm-only"},
		{"gpu missing", func(m *Manifest) { m.GPU = GPU{} }, "gpu"},
		{"gpu zero nodes", func(m *Manifest) { m.GPU.Nodes = 0 }, "gpu"},
		{"context", func(m *Manifest) { m.ContextMax = 0 }, "context_max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := valid()
			tc.mut(m)
			err := m.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateS3StreamOnVLLM(t *testing.T) {
	m := valid()
	m.Runtime = RuntimeVLLM
	m.Load = LoadS3Stream
	if err := m.Validate(); err != nil {
		t.Fatalf("s3-stream on vllm should validate: %v", err)
	}
}

func TestValidateCustomRuntime(t *testing.T) {
	m := valid()
	m.Runtime = RuntimeCustom
	m.Image = "ghcr.io/latere-ai/dotsocr:v1"
	m.Args = nil
	if err := m.Validate(); err != nil {
		t.Fatalf("custom runtime with image should validate: %v", err)
	}
}

func TestValidateRequiredArgsMiniMax(t *testing.T) {
	m := valid()
	m.Name = "minimax-m3"
	m.HFRepo = "MiniMaxAI/MiniMax-M3-MXFP8"
	m.S3Prefix = "s3://latere-models/MiniMaxAI/MiniMax-M3-MXFP8/" + sha + "/"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "--block-size=128") {
		t.Fatalf("MiniMax without --block-size=128 must be rejected, got %v", err)
	}
	m.Args = append(m.Args, "--block-size=128")
	if err := m.Validate(); err != nil {
		t.Fatalf("MiniMax with --block-size=128 rejected: %v", err)
	}
	// Split "--flag value" form also counts.
	m.Args = []string{"--tp-size=8", "--block-size", "128"}
	if err := m.Validate(); err != nil {
		t.Fatalf("split-form arg rejected: %v", err)
	}
}

// flashManifest is DeepSeek-V4-Flash-0731's shape (specs/017) — the
// only checkpoint in the fleet that runs speculative decoding.
func flashManifest() *Manifest {
	m := valid()
	m.Name = "deepseek-v4-flash-0731"
	m.HFRepo = "deepseek-ai/DeepSeek-V4-Flash-0731"
	m.S3Prefix = "s3://latere-models/deepseek-ai/DeepSeek-V4-Flash-0731/" + sha + "/"
	m.Args = []string{"--trust-remote-code", "--tp-size=8", "--speculative-algorithm=DSPARK"}
	return m
}

func TestValidateRequiredArgsTrustRemoteCode(t *testing.T) {
	for _, repo := range []string{"moonshotai/Kimi-K3", "deepseek-ai/DeepSeek-V4-Flash-0731"} {
		m := valid()
		m.HFRepo = repo
		m.S3Prefix = "s3://latere-models/" + repo + "/" + sha + "/"
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "--trust-remote-code") {
			t.Fatalf("%s without --trust-remote-code must be rejected, got %v", repo, err)
		}
		m.Args = append(m.Args, "--trust-remote-code")
		if err := m.Validate(); err != nil {
			t.Fatalf("%s with --trust-remote-code rejected: %v", repo, err)
		}
	}
}

func TestValidateDSpark(t *testing.T) {
	if err := flashManifest().Validate(); err != nil {
		t.Fatalf("DSpark manifest rejected: %v", err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"separate draft path",
			[]string{"--speculative-draft-model-path=/cache/draft"},
			"--speculative-draft-model-path",
		},
		{"pipeline parallel", []string{"--pp-size=2"}, "--pp-size"},
		{"pipeline parallel split form", []string{"--pp-size", "2"}, "--pp-size"},
		{"dp attention", []string{"--enable-dp-attention"}, "--enable-dp-attention"},
		{"dp attention with degree", []string{"--enable-dp-attention", "4"}, "--enable-dp-attention"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := flashManifest()
			m.Args = append(m.Args, tc.args...)
			err := m.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// The same flags are legal without DSpark — the rules are scoped to
	// the algorithm, not banned outright.
	m := flashManifest()
	m.Args = []string{"--trust-remote-code", "--pp-size=2", "--enable-dp-attention"}
	if err := m.Validate(); err != nil {
		t.Fatalf("non-DSpark manifest rejected: %v", err)
	}
	// A different algorithm does not trip the DSpark rules either.
	m.Args = []string{"--trust-remote-code", "--speculative-algorithm", "EAGLE",
		"--speculative-draft-model-path=/cache/draft"}
	if err := m.Validate(); err != nil {
		t.Fatalf("EAGLE with a draft path rejected: %v", err)
	}
}

func TestEngineImage(t *testing.T) {
	m := valid()
	if got := m.EngineImage(); got != "open-llms-runtime-sglang" {
		t.Errorf("sglang EngineImage = %q", got)
	}
	m.Runtime = RuntimeVLLM
	if got := m.EngineImage(); got != "open-llms-runtime-vllm" {
		t.Errorf("vllm EngineImage = %q", got)
	}
	m.Runtime = RuntimeCustom
	if got := m.EngineImage(); got != "" {
		t.Errorf("custom EngineImage = %q, want empty", got)
	}

	// K3 runs on the CUDA 13 branch build, not the shared image.
	k3 := valid()
	k3.HFRepo = "moonshotai/Kimi-K3"
	if got := k3.EngineImage(); got != "open-llms-runtime-sglang-k3" {
		t.Errorf("Kimi-K3 EngineImage = %q", got)
	}
}

// localManifest serves weights already present on the host's disk. The
// directory is not named here: it is <weights-root>/<hf_repo>/<revision>
// with the root supplied per host, so the manifest stays portable.
func localManifest() *Manifest {
	m := valid()
	m.Load = LoadLocal
	m.S3Prefix = ""
	return m
}

func TestValidateLocalOK(t *testing.T) {
	if err := localManifest().Validate(); err != nil {
		t.Fatalf("valid local manifest rejected: %v", err)
	}
}

func TestValidateLocalRejectsS3Prefix(t *testing.T) {
	// Two sources for one set of weights is the ambiguity the schema
	// exists to prevent, so this is an error rather than a precedence
	// rule.
	m := localManifest()
	m.S3Prefix = "s3://latere-models/moonshotai/Kimi-K2.7-Code/" + sha + "/"
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "s3_prefix must be empty") {
		t.Fatalf("load: local with an s3_prefix accepted: %v", err)
	}
}

func TestValidateS3ModesStillRequirePrefix(t *testing.T) {
	// Adding a third mode must not weaken the two that existed.
	for _, load := range []string{LoadNVMeCache, LoadS3Stream} {
		m := valid()
		m.Runtime = RuntimeVLLM
		m.Load = load
		m.S3Prefix = ""
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "s3://") {
			t.Errorf("load %s without s3_prefix accepted: %v", load, err)
		}
	}
}

// gb10Manifest is a minimal valid single-GPU unified-memory model.
func gb10Manifest() *Manifest {
	m := valid()
	m.Runtime = RuntimeVLLM
	m.Deploy = DeployBareMetal
	m.GPU = GPU{Type: GPUTypeGB10, Count: 1, Nodes: 1}
	m.Args = []string{"--max-model-len=262144", "--gpu-memory-utilization=0.65"}
	return m
}

func TestValidateGB10OK(t *testing.T) {
	if err := gb10Manifest().Validate(); err != nil {
		t.Fatalf("valid gb10 manifest rejected: %v", err)
	}
	// The SGLang flag names differ; both engines must be expressible.
	m := gb10Manifest()
	m.Runtime = RuntimeSGLang
	m.Args = []string{"--context-length=262144", "--mem-fraction-static=0.80"}
	if err := m.Validate(); err != nil {
		t.Fatalf("valid sglang gb10 manifest rejected: %v", err)
	}
}

func TestValidateGB10Rejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Manifest)
		want string
	}{
		{"two gpus", func(m *Manifest) { m.GPU.Count = 2 }, "single-GPU"},
		{"two nodes", func(m *Manifest) { m.GPU.Nodes = 2 }, "single-GPU"},
		{"no fraction", func(m *Manifest) {
			m.Args = []string{"--max-model-len=262144"}
		}, "--gpu-memory-utilization"},
		{"fraction too high", func(m *Manifest) {
			m.Args = []string{"--max-model-len=262144", "--gpu-memory-utilization=0.90"}
		}, "starves the host"},
		{"fraction not a number", func(m *Manifest) {
			m.Args = []string{"--max-model-len=262144", "--gpu-memory-utilization=most"}
		}, "not a number"},
		{"fraction negative", func(m *Manifest) {
			m.Args = []string{"--max-model-len=262144", "--gpu-memory-utilization=-0.5"}
		}, "must be positive"},
		{"no context bound", func(m *Manifest) {
			m.Args = []string{"--gpu-memory-utilization=0.65"}
		}, "--max-model-len"},
		{"sglang flags on vllm", func(m *Manifest) {
			m.Args = []string{"--context-length=262144", "--mem-fraction-static=0.65"}
		}, "--gpu-memory-utilization"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := gb10Manifest()
			tc.mut(m)
			err := m.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateGB10BoundaryFraction(t *testing.T) {
	// Exactly at the ceiling is allowed; a hair over is not. The
	// boundary is the whole point of the rule, so it is pinned.
	m := gb10Manifest()
	m.Args = []string{"--max-model-len=4096", "--gpu-memory-utilization=0.80"}
	if err := m.Validate(); err != nil {
		t.Fatalf("fraction at the ceiling rejected: %v", err)
	}
	m.Args = []string{"--max-model-len=4096", "--gpu-memory-utilization=0.81"}
	if err := m.Validate(); err == nil {
		t.Fatal("fraction above the ceiling accepted")
	}
}

func TestValidateGB10RulesAreScoped(t *testing.T) {
	// Fleet models must not inherit any of this: they have discrete
	// HBM, and an unset fraction there is a tuning choice, not a bug.
	m := valid() // h200, sglang, args are just --tp-size=8
	if err := m.Validate(); err != nil {
		t.Fatalf("non-gb10 manifest caught by gb10 rules: %v", err)
	}
}

func TestDeployMode(t *testing.T) {
	// Absent means k8s, so every manifest written before the field
	// existed keeps its meaning without an edit.
	m := valid()
	if got := m.DeployMode(); got != DeployK8s {
		t.Errorf("absent deploy = %q, want %q", got, DeployK8s)
	}
	if got := m.DeployArtifact(); got != "kimi-k2.7-code/lws.yaml" {
		t.Errorf("k8s artifact = %q", got)
	}

	m.Deploy = DeployBareMetal
	if err := m.Validate(); err != nil {
		t.Fatalf("bare-metal rejected: %v", err)
	}
	if got := m.DeployArtifact(); got != "kimi-k2.7-code/kimi-k2.7-code.service" {
		t.Errorf("bare-metal artifact = %q", got)
	}

	m.Deploy = "nomad"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Errorf("unknown deploy mode accepted: %v", err)
	}
}

func TestDeployBareMetalRejectsImage(t *testing.T) {
	// runtime: custom is the only way to set Image, and bare-metal
	// builds no container for it to name.
	m := valid()
	m.Deploy = DeployBareMetal
	m.Runtime = RuntimeCustom
	m.Image = "ghcr.io/latere-ai/whatever:v1"
	m.Args = nil
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "no container") {
		t.Fatalf("bare-metal with image accepted: %v", err)
	}
}

func TestFlagValue(t *testing.T) {
	m := valid()
	m.Args = []string{"--a=1", "--b", "2", "--c", "--d", "--e="}
	cases := []struct {
		flag  string
		value string
		ok    bool
	}{
		{"--a", "1", true},
		{"--b", "2", true},
		{"--c", "", true}, // valueless: next token is another flag
		{"--d", "", true}, // valueless: end of args
		{"--e", "", true}, // explicitly empty
		{"--f", "", false},
		{"--", "", false},
	}
	for _, tc := range cases {
		v, ok := m.FlagValue(tc.flag)
		if v != tc.value || ok != tc.ok {
			t.Errorf("FlagValue(%q) = (%q, %v), want (%q, %v)", tc.flag, v, ok, tc.value, tc.ok)
		}
	}
}

func TestHasArg(t *testing.T) {
	m := valid()
	m.Args = []string{"--a=1", "--b", "2", "--c"}
	for _, want := range []string{"--a=1", "--b=2", "--c"} {
		if !m.HasArg(want) {
			t.Errorf("HasArg(%q) = false, want true", want)
		}
	}
	for _, not := range []string{"--a=2", "--b=3", "--d"} {
		if m.HasArg(not) {
			t.Errorf("HasArg(%q) = true, want false", not)
		}
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("name: x\nbogus_field: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "bogus_field") {
		t.Fatalf("unknown field must be rejected, got %v", err)
	}
}

func TestParseRejectsBadYAML(t *testing.T) {
	if _, err := Parse([]byte("{:")); err == nil {
		t.Fatal("bad yaml must be rejected")
	}
}

func writeManifest(t *testing.T, dir, name string, m *Manifest) string {
	t.Helper()
	p := filepath.Join(dir, name)
	data := "name: " + m.Name + "\n" +
		"hf_repo: " + m.HFRepo + "\n" +
		"revision: " + m.Revision + "\n" +
		"s3_prefix: " + m.S3Prefix + "\n" +
		"format: " + m.Format + "\n" +
		"license: " + m.License + "\n" +
		"runtime: " + m.Runtime + "\n" +
		"load: " + m.Load + "\n" +
		"gpu: {type: h200, count: 8, nodes: 1}\n" +
		"context_max: 262144\n" +
		"args: [\"--tp-size=8\"]\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "kimi.yaml", valid())

	m, err := Load(filepath.Join(dir, "kimi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "kimi-k2.7-code" {
		t.Fatalf("loaded name %q", m.Name)
	}

	ms, err := LoadDir(dir)
	if err != nil || len(ms) != 1 {
		t.Fatalf("LoadDir = %v, %v", ms, err)
	}

	// Invalid file poisons the dir.
	bad := valid()
	bad.Revision = "main"
	writeManifest(t, dir, "bad.yaml", bad)
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("LoadDir must fail on invalid manifest")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestValidateSystemPrompt(t *testing.T) {
	for _, mode := range []string{SystemPromptDefault, SystemPromptPrepend, SystemPromptOverride} {
		m := valid()
		m.SystemPrompt = &SystemPrompt{Mode: mode, Text: "be helpful"}
		if err := m.Validate(); err != nil {
			t.Fatalf("mode %s rejected: %v", mode, err)
		}
	}
	m := valid()
	m.SystemPrompt = &SystemPrompt{Mode: "sometimes", Text: "x"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "system_prompt.mode") {
		t.Fatalf("bad mode not rejected: %v", err)
	}
	m.SystemPrompt = &SystemPrompt{Mode: SystemPromptDefault, Text: "   "}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "system_prompt.text") {
		t.Fatalf("empty text not rejected: %v", err)
	}
}
