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
