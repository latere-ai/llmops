// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The local e2e script (specs/011) is the only caller of the shim that
// lives outside Go, so nothing recompiles when a route moves. specs/025
// moved the Anthropic surface from /anthropic/v1/messages to
// /v1/messages and the script kept the old path, which fails only on a
// machine that can run mlx_lm.
//
// This ties the script to the route table: every shim path it curls must
// be one the shim answers.
func TestE2EScriptCallsOnlyServedPaths(t *testing.T) {
	// go test runs in the package directory, so the repo root is two
	// levels up. runtime.Caller would return a trimmed path under
	// -trimpath and fail here for a reason that reads like a missing file.
	script := filepath.Join("..", "..", "e2e", "local", "run.sh")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}

	// Paths served by the shim itself, beyond the caller dialects.
	served := map[string]bool{
		"/healthz": true, "/ready": true, "/metrics": true, "/v1/models": true,
	}
	for _, f := range frontends {
		served[f.path] = true
	}

	// $PORT is the shim; other ports (MinIO, the engine) are not its
	// business.
	re := regexp.MustCompile(`\$PORT(/[A-Za-z0-9/_.-]*)`)
	found := 0
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		found++
		if !served[m[1]] {
			t.Errorf("e2e/local/run.sh calls %s, which the shim does not serve", m[1])
		}
	}
	if found == 0 {
		t.Fatal("no shim URLs found in run.sh; the regexp or the script changed shape")
	}
}
