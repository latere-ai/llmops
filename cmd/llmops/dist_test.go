// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCrossCompileTargetsRequestedArch pins the one thing `make dist`
// can get silently wrong (specs/020 AC3).
//
// A plain `go build` targets the builder, so building on a laptop for
// an arm64 host emits an amd64 binary that installs fine and dies with
// "exec format error" on first start. This asserts GOARCH is honoured
// and that the result is a static ELF, since CGO would otherwise pull
// in a dynamic loader the target host may not match.
func TestCrossCompileTargetsRequestedArch(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles two binaries")
	}
	for _, tc := range []struct {
		goarch string
		want   elf.Machine
	}{
		{"arm64", elf.EM_AARCH64},
		{"amd64", elf.EM_X86_64},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "runtime")
			cmd := exec.Command("go", "build", "-o", out, ".")
			cmd.Env = append(os.Environ(),
				"GOOS=linux", "GOARCH="+tc.goarch, "CGO_ENABLED=0")
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build %s: %v\n%s", tc.goarch, err, b)
			}
			f, err := elf.Open(out)
			if err != nil {
				t.Fatalf("open %s: %v", tc.goarch, err)
			}
			defer func() { _ = f.Close() }()
			if f.Machine != tc.want {
				t.Errorf("GOARCH=%s produced %v, want %v", tc.goarch, f.Machine, tc.want)
			}
			// A static binary has no interpreter to mismatch on the host.
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					t.Errorf("GOARCH=%s binary is dynamically linked", tc.goarch)
				}
			}
		})
	}
}
