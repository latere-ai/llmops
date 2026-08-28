package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/llmops/internal/mirror"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// startHub serves a one-file fake HF repo and fakes `hf` downloads by
// pre-materializing the scratch dir (Pull verifies and skips download,
// so the hf binary is never invoked).
func startHub(t *testing.T) (scratch string) {
	t.Helper()
	scratch = t.TempDir()
	content := "tiny-weights"
	if err := os.WriteFile(filepath.Join(scratch, "model.safetensors"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := mirror.FileSHA256(filepath.Join(scratch, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/acme/tiny", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"sha": testSHA})
	})
	mux.HandleFunc("/api/models/acme/tiny/revision/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"sha": testSHA})
	})
	mux.HandleFunc("/api/models/acme/tiny/tree/"+testSHA, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{
			"type": "file", "path": "model.safetensors", "size": len(content),
			"lfs": map[string]any{"oid": "sha256:" + sum},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("LLMOPS_HF_BASE", srv.URL)
	return scratch
}

func TestCLIPullPushVerifyLs(t *testing.T) {
	scratch := startHub(t)
	bucket := t.TempDir()
	var out, errw strings.Builder

	if code := run([]string{"pull", "acme/tiny", "--dir", scratch}, &out, &errw); code != 0 {
		t.Fatalf("pull exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), testSHA) {
		t.Fatalf("pull output missing sha: %s", out.String())
	}

	if code := run([]string{"push", "acme/tiny@" + testSHA, "--dir", scratch, "--bucket", bucket}, &out, &errw); code != 0 {
		t.Fatalf("push exit %d: %s", code, errw.String())
	}

	prefix := filepath.Join(bucket, "acme/tiny", testSHA)
	if code := run([]string{"verify", prefix}, &out, &errw); code != 0 {
		t.Fatalf("verify exit %d: %s", code, errw.String())
	}

	out.Reset()
	if code := run([]string{"list", "--bucket", bucket}, &out, &errw); code != 0 {
		t.Fatalf("list exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "acme/tiny/"+testSHA) {
		t.Fatalf("ls output missing mirror: %q", out.String())
	}
}

func TestCLIPullFreezeVerify(t *testing.T) {
	// The bare-metal path has no bucket: pull, freeze in place, and the
	// weights directory is itself a verifiable store (specs/021).
	weights := startHub(t)
	var out, errw strings.Builder

	if code := run([]string{"pull", "acme/tiny", "--dir", weights}, &out, &errw); code != 0 {
		t.Fatalf("pull exit %d: %s", code, errw.String())
	}
	out.Reset()
	if code := run([]string{"freeze", "acme/tiny@" + testSHA, "--dir", weights}, &out, &errw); code != 0 {
		t.Fatalf("freeze exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), testSHA) {
		t.Fatalf("freeze output missing sha: %q", out.String())
	}
	if code := run([]string{"verify", weights}, &out, &errw); code != 0 {
		t.Fatalf("verify of frozen dir exit %d: %s", code, errw.String())
	}
}

func TestCLIFreezeUsage(t *testing.T) {
	var out, errw strings.Builder
	cases := [][]string{
		{"freeze"},
		{"freeze", "acme/tiny@" + testSHA},            // no --dir
		{"freeze", "acme/tiny", "--dir", t.TempDir()}, // unpinned revision
	}
	for _, args := range cases {
		if code := run(args, &out, &errw); code == 0 {
			t.Fatalf("args %v must fail", args)
		}
	}
}

func TestCLIVerifyDetectsCorruption(t *testing.T) {
	scratch := startHub(t)
	bucket := t.TempDir()
	var out, errw strings.Builder
	if code := run([]string{"push", "acme/tiny@" + testSHA, "--dir", scratch, "--bucket", bucket}, &out, &errw); code != 0 {
		t.Fatalf("push exit %d: %s", code, errw.String())
	}
	stored := filepath.Join(bucket, "acme/tiny", testSHA, "model.safetensors")
	if err := os.WriteFile(stored, []byte("tiny-weightX"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"verify", filepath.Join(bucket, "acme/tiny", testSHA)}, &out, &errw); code == 0 {
		t.Fatal("verify must fail on corrupted store")
	}
}

func TestCLIUsageErrors(t *testing.T) {
	var out, errw strings.Builder
	cases := [][]string{
		{},
		{"bogus"},
		{"pull"},
		{"pull", "acme/tiny"},
		{"push", "acme/tiny@" + testSHA},
		{"push", "acme/tiny", "--dir", "d", "--bucket", "b"},
		{"verify"},
		{"list"},
	}
	for _, args := range cases {
		if code := run(args, &out, &errw); code == 0 {
			t.Fatalf("args %v must fail", args)
		}
	}
}

func TestCLIPullError(t *testing.T) {
	t.Setenv("LLMOPS_HF_BASE", "http://127.0.0.1:1")
	var out, errw strings.Builder
	if code := run([]string{"pull", "acme/tiny", "--dir", t.TempDir()}, &out, &errw); code == 0 {
		t.Fatal("pull against dead hub must fail")
	}
}
