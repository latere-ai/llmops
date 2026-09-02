// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package install

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/llmops/internal/manifest"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func bareMetalManifest(t *testing.T, dir string) (*manifest.Manifest, string) {
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
	m, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestUnitNamesBinaryManifestAndServe(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())
	u := Unit(m, Options{CacheRoot: "/var/models"})
	for _, want := range []string{
		"ExecStart=" + DefaultBinPath + " serve --manifest " + DefaultConfigDir + "/qwen.yaml",
		"--cache-root /var/models",
		"TimeoutStartSec=" + DefaultStartTimeout,
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q:\n%s", want, u)
		}
	}
}

func TestUnitOmitsCacheRootWhenUnset(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())
	if strings.Contains(Unit(m, Options{}), "--cache-root") {
		t.Error("empty CacheRoot must not emit the flag")
	}
}

// TestUnitStartTimeoutOutrunsWeightLoad guards the value, not just its
// presence: systemd's 90 s default would kill a large model mid-load
// and restart it forever, which reads as a crash rather than a timeout
// (specs/019 measured 325 s for a 0.6B model).
func TestUnitStartTimeoutOutrunsWeightLoad(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())
	if !strings.Contains(Unit(m, Options{}), "TimeoutStartSec=30min") {
		t.Error("start timeout is not generous enough for weight load")
	}
}

func TestRunIsIdempotent(t *testing.T) {
	src := t.TempDir()
	m, p := bareMetalManifest(t, src)
	root := t.TempDir()
	opts := Options{
		BinPath:   filepath.Join(root, "bin", "llmops"),
		ConfigDir: filepath.Join(root, "etc"),
		UnitDir:   filepath.Join(root, "units"),
	}

	reloads := 0
	opts.Reload = func(io.Writer) error { reloads++; return nil }

	first, err := Run(m, p, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ManifestChanged || !first.UnitChanged {
		t.Fatal("first install reported no writes")
	}
	if !first.DaemonReloaded || reloads != 1 {
		t.Fatalf("first install reloaded %d times, want 1", reloads)
	}

	// A second identical run must change nothing, so systemd is not
	// reloaded and mtimes do not churn.
	second, err := Run(m, p, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second.ManifestChanged || second.UnitChanged || second.DaemonReloaded {
		t.Fatalf("second install was not a no-op: %+v", second)
	}
	if reloads != 1 {
		t.Fatalf("a no-op install reloaded systemd (%d times total)", reloads)
	}
}

// TestRunSurvivesReloadFailure pins that a reload we are not allowed to
// perform does not fail the install. The files are already correct; an
// unprivileged install into a staging directory is legitimate, and CI
// runners have a systemctl that refuses rather than one that is absent.
func TestRunSurvivesReloadFailure(t *testing.T) {
	src := t.TempDir()
	m, p := bareMetalManifest(t, src)
	root := t.TempDir()
	var log strings.Builder
	res, err := Run(m, p, Options{
		ConfigDir: filepath.Join(root, "etc"),
		UnitDir:   filepath.Join(root, "units"),
		Reload:    func(io.Writer) error { return errors.New("Interactive authentication required") },
	}, &log)
	if err != nil {
		t.Fatalf("reload failure made the install fail: %v", err)
	}
	if !res.UnitChanged {
		t.Fatal("unit was not written")
	}
	if res.DaemonReloaded {
		t.Fatal("reported a reload that failed")
	}
	if !strings.Contains(log.String(), "systemctl daemon-reload") {
		t.Fatalf("did not tell the operator what to run: %q", log.String())
	}
	if _, err := os.Stat(res.UnitPath); err != nil {
		t.Fatalf("unit missing after a failed reload: %v", err)
	}
}

func TestRunUpdatesChangedManifest(t *testing.T) {
	src := t.TempDir()
	m, p := bareMetalManifest(t, src)
	root := t.TempDir()
	opts := Options{
		BinPath:   filepath.Join(root, "bin", "llmops"),
		ConfigDir: filepath.Join(root, "etc"),
		UnitDir:   filepath.Join(root, "units"),
		Reload:    func(io.Writer) error { return nil },
	}
	if _, err := Run(m, p, opts, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Edit the source manifest; the installed copy must follow rather
	// than a second one appearing beside it.
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "context_max: 4096", "context_max: 8192", 1)
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(m2, p, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ManifestChanged {
		t.Fatal("changed manifest was not reinstalled")
	}
	installed, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "context_max: 8192") {
		t.Fatal("installed manifest is stale")
	}
	entries, err := os.ReadDir(opts.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("install duplicated files: %d entries", len(entries))
	}
}

func TestRunRejectsK8sModel(t *testing.T) {
	// Installing a fleet model as a unit would start it outside the
	// scheduler that is supposed to own it.
	dir := t.TempDir()
	p := filepath.Join(dir, "k8s.yaml")
	data := `name: k8s
hf_repo: acme/tiny
revision: ` + sha + `
s3_prefix: s3://b/acme/tiny/` + sha + `/
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
	m, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(m, p, Options{ConfigDir: dir, UnitDir: dir,
		Reload: func(io.Writer) error { return nil }}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "bare-metal") {
		t.Fatalf("k8s model accepted by install: %v", err)
	}
}

// TestDaemonReloadWithoutSystemctl covers the path a machine without
// systemd takes: rendering the files is still useful when staging them
// for another host, so a missing systemctl is not an error.
func TestDaemonReloadWithoutSystemctl(t *testing.T) {
	// An empty PATH guarantees the lookup fails, whatever the host has.
	t.Setenv("PATH", "")
	var log strings.Builder
	if err := daemonReload(&log); err != nil {
		t.Fatalf("a missing systemctl must not be an error: %v", err)
	}
	if !strings.Contains(log.String(), "systemctl not found") {
		t.Fatalf("the skip was silent: %q", log.String())
	}
}

// TestDaemonReloadReportsALookupThatIsNotAnAbsence is the other half of
// specs/020's "a host without systemd is not an error": a lookup that fails
// for any other reason is one. Here PATH names a relative directory, which
// exec.LookPath resolves and then refuses with ErrDot rather than running.
// Reporting that as "systemctl not found" would tell an operator their host
// has no systemd when it has one this process declined to invoke.
func TestDaemonReloadReportsALookupThatIsNotAnAbsence(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "systemctl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", "bin")
	var log strings.Builder
	err := daemonReload(&log)
	if err == nil {
		t.Fatal("a systemctl that was found and refused was reported as absent")
	}
	if !errors.Is(err, exec.ErrDot) {
		t.Fatalf("daemonReload = %v, want the lookup error", err)
	}
	if strings.Contains(log.String(), "systemctl not found") {
		t.Fatalf("a refused lookup was logged as an absence: %q", log.String())
	}
}
