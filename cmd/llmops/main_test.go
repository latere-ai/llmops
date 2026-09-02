// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

func TestUsageListsEverySubcommand(t *testing.T) {
	// The usage text and the dispatch switch are written separately, so
	// a verb added to one and not the other is invisible until someone
	// tries it. This is the test that makes that loud.
	for _, cmd := range dispatch {
		if !strings.Contains(usage, cmd) {
			t.Errorf("subcommand %q is dispatched but missing from usage", cmd)
		}
	}
	var out, errw strings.Builder
	if code := run(nil, &out, &errw); code != 2 {
		t.Errorf("no arguments: exit %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "usage: llmops") {
		t.Errorf("no arguments did not print usage: %q", errw.String())
	}
}

func TestEveryDispatchedCommandIsReachable(t *testing.T) {
	// Called with no flags, each verb must fail as a *usage* error (2),
	// not as an unknown command (which also exits 2 but prints the
	// wrong thing) and not by panicking. `version` is the exception: it
	// needs no arguments and must succeed.
	for _, cmd := range dispatch {
		t.Run(cmd, func(t *testing.T) {
			var out, errw strings.Builder
			code := run([]string{cmd}, &out, &errw)
			if cmd == "version" {
				if code != 0 {
					t.Fatalf("version exit %d: %s", code, errw.String())
				}
				if strings.TrimSpace(out.String()) == "" {
					t.Fatal("version printed nothing")
				}
				return
			}
			if strings.Contains(errw.String(), "unknown command") {
				t.Fatalf("%q is in dispatch but not routed: %s", cmd, errw.String())
			}
		})
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	// Exit 2 separates "you invoked this wrong" from "the work failed",
	// which a script or a systemd unit can act on differently.
	var out, errw strings.Builder
	code := run([]string{"mirror", "pull", "acme/tiny"}, &out, &errw)
	if code != 2 {
		t.Fatalf("stale `mirror` grouping: exit %d, want 2", code)
	}
	if !strings.Contains(errw.String(), `unknown command "mirror"`) {
		t.Fatalf("error did not name the unknown command: %q", errw.String())
	}
}

func TestVersionReportsSomething(t *testing.T) {
	var out, errw strings.Builder
	if code := run([]string{"version"}, &out, &errw); code != 0 {
		t.Fatalf("version exit %d", code)
	}
	// A `go test` binary has build info but no ldflags, so this asserts
	// the fallback path rather than a stamped value.
	if got := strings.TrimSpace(out.String()); got == "" {
		t.Fatal("version is empty")
	}
}
