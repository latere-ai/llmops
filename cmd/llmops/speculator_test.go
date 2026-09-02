// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFastPath writes the shape specs/027 introduces: one manifest
// offering two draft heads, with the choice made at start time.
func writeFastPath(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "qwen-fast.yaml")
	data := `name: qwen-fast
hf_repo: acme/tiny
revision: ` + sha + `
format: nvfp4
license: apache-2.0
runtime: sglang
deploy: bare-metal
load: local
gpu: {type: gb10, count: 1, nodes: 1}
context_max: 4096
args: ["--context-length=4096", "--mem-fraction-static=0.80"]
default_speculator: dspark
speculators:
  dspark:
    hf_repo: acme/tiny-dspark
    revision: ` + sha + `
    license: other
    args: ["--speculative-algorithm=DSPARK"]
  dflash2:
    hf_repo: acme/tiny-dflash2
    revision: ` + sha + `
    license: apache-2.0
    args: ["--speculative-algorithm=DFLASH"]
`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func installFast(t *testing.T, p, root string, extra ...string) (string, string, int) {
	t.Helper()
	args := append([]string{"install", "--manifest", p, "--no-reload",
		"--config-dir", filepath.Join(root, "etc"),
		"--unit-dir", filepath.Join(root, "units"),
	}, extra...)
	var out, errw strings.Builder
	code := run(args, &out, &errw)
	return out.String(), errw.String(), code
}

// TestInstallPinsTheChosenSpeculator: the operator's choice has to
// survive a restart, so it is written into the unit rather than left to
// the manifest's default.
func TestInstallPinsTheChosenSpeculator(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	p := writeFastPath(t, src)

	if _, errw, code := installFast(t, p, root, "--speculator", "dflash2"); code != 0 {
		t.Fatalf("install exit %d: %s", code, errw)
	}
	unit, err := os.ReadFile(filepath.Join(root, "units", "qwen-fast.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "--speculator dflash2") {
		t.Fatalf("unit does not pin the choice:\n%s", unit)
	}
}

// TestInstallRejectsAnUnknownSpeculatorBeforeWriting: a name that does
// not exist must fail here, not at the first `systemctl start` of a unit
// that looks installed.
func TestInstallRejectsAnUnknownSpeculatorBeforeWriting(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	p := writeFastPath(t, src)

	_, errw, code := installFast(t, p, root, "--speculator", "eagle")
	if code == 0 {
		t.Fatal("an unknown speculator was installed")
	}
	for _, want := range []string{"dspark", "dflash2"} {
		if !strings.Contains(errw, want) {
			t.Errorf("error does not offer %q: %s", want, errw)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "units", "qwen-fast.service")); !os.IsNotExist(err) {
		t.Fatal("a unit was written despite the invalid choice")
	}
}

// TestInstallWithoutASpeculatorLeavesTheDefaultToTheManifest keeps the
// manifest authoritative when the operator expresses no preference.
func TestInstallWithoutASpeculatorLeavesTheDefaultToTheManifest(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	p := writeFastPath(t, src)

	if _, errw, code := installFast(t, p, root); code != 0 {
		t.Fatalf("install exit %d: %s", code, errw)
	}
	unit, err := os.ReadFile(filepath.Join(root, "units", "qwen-fast.service"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "--speculator") {
		t.Fatalf("unit pinned a choice the operator did not make:\n%s", unit)
	}
}

// TestServeRejectsAnUnknownSpeculator: the check happens before weights
// are touched, so a typo costs nothing.
func TestServeRejectsAnUnknownSpeculator(t *testing.T) {
	p := writeFastPath(t, t.TempDir())
	var out, errw strings.Builder
	code := run([]string{"serve", "--manifest", p, "--speculator", "eagle"}, &out, &errw)
	if code == 0 {
		t.Fatal("serve accepted an unknown speculator")
	}
	if !strings.Contains(errw.String(), "no speculator") {
		t.Fatalf("error does not name the problem: %s", errw.String())
	}
}

// TestPSShowsTheSpeculatorColumn: a throughput number that does not say
// which draft head produced it cannot be attributed (specs/027 AC3).
func TestPSShowsTheSpeculatorColumn(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "etc")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	p := writeFastPath(t, src)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "qwen-fast.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw strings.Builder
	code := run([]string{"ps", "--config-dir", cfg, "--unit-dir", filepath.Join(root, "units"),
		"--timeout", "100ms"}, &out, &errw)
	if code != 0 {
		t.Fatalf("ps exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "SPECULATOR") {
		t.Fatalf("ps has no speculator column:\n%s", out.String())
	}
	// Nothing is serving, so the value is unknown rather than "none".
	if !strings.Contains(out.String(), "-") {
		t.Fatalf("ps did not mark the speculator unknown:\n%s", out.String())
	}
}
