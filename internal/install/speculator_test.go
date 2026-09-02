// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package install

import (
	"strings"
	"testing"
)

// TestUnitPinsTheSpeculator: which draft head runs is chosen when the
// model is started, so a unit that omitted it would silently serve the
// manifest's default after the next restart (specs/027).
func TestUnitPinsTheSpeculator(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())

	u := Unit(m, Options{CacheRoot: "/var/models", Speculator: "dflash2"})
	if !strings.Contains(u, "--speculator dflash2") {
		t.Fatalf("unit does not pin the speculator:\n%s", u)
	}

	// Omitted, the manifest decides — the flag is absent rather than
	// written as an empty value the engine would reject.
	u = Unit(m, Options{CacheRoot: "/var/models"})
	if strings.Contains(u, "--speculator") {
		t.Fatalf("unit invented a speculator flag:\n%s", u)
	}
}
