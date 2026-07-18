package main

import (
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

func TestValidateFileAndDir(t *testing.T) {
	dir := t.TempDir()
	p := writeValid(t, dir)
	var out, errw strings.Builder

	if code := run([]string{"validate", p}, &out, &errw); code != 0 {
		t.Fatalf("validate file exit %d: %s", code, errw.String())
	}
	if code := run([]string{"validate", dir}, &out, &errw); code != 0 {
		t.Fatalf("validate dir exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "1 manifests valid") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestValidateRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("name: bad\nrevision: main\n"), 0o644)
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
	t.Setenv("OPENLLMS_ENGINE_CMD", "sleep 60")
	var out, errw strings.Builder
	code := run([]string{"serve", "--manifest", p, "--port", "0", "--cache-root", t.TempDir()}, &out, &errw)
	if code == 0 {
		t.Fatal("serve against unreachable store must fail")
	}
	if !strings.Contains(errw.String(), "prepare weights") {
		t.Fatalf("expected prep failure, got: %s", errw.String())
	}
}
