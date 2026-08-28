package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBareMetal(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "qwen.yaml")
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
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallWritesAndIsIdempotent(t *testing.T) {
	src := t.TempDir()
	root := t.TempDir()
	p := writeBareMetal(t, src)
	args := []string{"install", "--manifest", p,
		"--config-dir", filepath.Join(root, "etc"),
		"--unit-dir", filepath.Join(root, "units"),
	}

	var out, errw strings.Builder
	if code := run(args, &out, &errw); code != 0 {
		t.Fatalf("install exit %d: %s", code, errw.String())
	}
	unit, err := os.ReadFile(filepath.Join(root, "units", "qwen.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "llmops serve --manifest") {
		t.Fatalf("unit does not start the service:\n%s", unit)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "qwen.yaml")); err != nil {
		t.Fatalf("manifest not installed: %v", err)
	}

	// Running it again must say so rather than implying work happened.
	out.Reset()
	if code := run(args, &out, &errw); code != 0 {
		t.Fatalf("second install exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("second install did not report a no-op: %q", out.String())
	}
}

func TestInstallPrintWritesNothing(t *testing.T) {
	src := t.TempDir()
	root := t.TempDir()
	p := writeBareMetal(t, src)
	var out, errw strings.Builder
	code := run([]string{"install", "--manifest", p, "--print",
		"--config-dir", filepath.Join(root, "etc"),
		"--unit-dir", filepath.Join(root, "units"),
	}, &out, &errw)
	if code != 0 {
		t.Fatalf("install --print exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "[Service]") {
		t.Fatalf("--print did not emit a unit: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "units")); !os.IsNotExist(err) {
		t.Fatal("--print wrote to the unit directory")
	}
}

func TestInstallRejectsK8sModel(t *testing.T) {
	// A fleet model installed as a unit would run outside the scheduler
	// that is supposed to own it.
	dir := t.TempDir()
	p := writeValid(t, dir) // h200, deploy defaults to k8s
	var out, errw strings.Builder
	if code := run([]string{"install", "--manifest", p}, &out, &errw); code == 0 {
		t.Fatal("install accepted a k8s model")
	}
	if !strings.Contains(errw.String(), "bare-metal") {
		t.Fatalf("error did not explain the mode: %q", errw.String())
	}
}

func TestInstallUsage(t *testing.T) {
	var out, errw strings.Builder
	if code := run([]string{"install"}, &out, &errw); code != 2 {
		t.Fatalf("install with no manifest: exit %d, want 2", code)
	}
}
