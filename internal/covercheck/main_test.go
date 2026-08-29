package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAverageCannotHideAPackage is the reason this command exists: a
// repository average lets a well-covered package carry an uncovered one.
// Here the average is 90% and one package is at 50%.
func TestAverageCannotHideAPackage(t *testing.T) {
	p := writeProfile(t, `mode: set
mod/good/a.go:1.1,2.1 90 1
mod/bad/b.go:1.1,2.1 5 1
mod/bad/b.go:3.1,4.1 5 0
`)
	byPkg, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	good, bad := byPkg["mod/good"], byPkg["mod/bad"]
	if good.covered != 90 || good.total != 90 {
		t.Fatalf("good package: %+v", good)
	}
	if bad.covered != 5 || bad.total != 10 {
		t.Fatalf("bad package: %+v", bad)
	}
	// The average would be 95/100 = 95%, comfortably over the floor.
	if err := run(p, io.Discard); err == nil {
		t.Fatal("a package at 50% passed behind a high average")
	}
}

// TestBlockCountedOnceAcrossTestBinaries pins the merge rule: with
// -coverpkg the same block appears once per test binary that executed
// it, and summing them would inflate the totals.
func TestBlockCountedOnceAcrossTestBinaries(t *testing.T) {
	p := writeProfile(t, `mode: set
mod/pkg/a.go:1.1,2.1 10 0
mod/pkg/a.go:1.1,2.1 10 1
`)
	byPkg, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	c := byPkg["mod/pkg"]
	if c.total != 10 {
		t.Fatalf("total %d, want 10: the same block was counted twice", c.total)
	}
	if c.covered != 10 {
		t.Fatalf("covered %d, want 10: a block covered by any run is covered", c.covered)
	}
}

func TestPassesWhenEveryPackageClears(t *testing.T) {
	p := writeProfile(t, `mode: set
mod/a/x.go:1.1,2.1 95 1
mod/a/x.go:3.1,4.1 5 0
mod/b/y.go:1.1,2.1 100 1
`)
	byPkg, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(byPkg) != 2 {
		t.Fatalf("packages: %v", byPkg)
	}
	var out strings.Builder
	if err := run(p, &out); err != nil {
		t.Fatalf("95%% and 100%% should pass: %v", err)
	}
	if !strings.Contains(out.String(), "every package clears") {
		t.Fatalf("no summary line: %q", out.String())
	}
}

func TestExemptionNeedsAReason(t *testing.T) {
	// The map's value *is* the reason, so an entry cannot exist without
	// one. This pins that the lookup reports it rather than discarding
	// it, which is what makes an exemption reviewable.
	exempt["mod/legacy"] = "vendored upstream; not ours to test"
	defer delete(exempt, "mod/legacy")

	why, ok := exemptFor("some/mod/legacy")
	if !ok || why == "" {
		t.Fatalf("exemption without a reason: %q %v", why, ok)
	}
	if _, ok := exemptFor("some/other"); ok {
		t.Fatal("unrelated package reported as exempt")
	}
}

func TestMissingProfileIsAnError(t *testing.T) {
	if _, err := parse(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing profile accepted")
	}
	empty := writeProfile(t, "mode: set\n")
	if err := run(empty, io.Discard); err == nil {
		t.Fatal("a profile covering nothing passed")
	}
}

func TestShortDropsTheModulePrefix(t *testing.T) {
	if got := short("github.com/latere-ai/llmops/internal/harness"); got != "internal/harness" {
		t.Fatalf("short = %q", got)
	}
	if got := short("standalone"); got != "standalone" {
		t.Fatalf("short = %q", got)
	}
}

func TestMalformedLinesAreSkipped(t *testing.T) {
	// A profile is machine-written, but a truncated one should not be
	// read as "everything is covered".
	p := writeProfile(t, `mode: set
garbage
mod/pkg/a.go:1.1,2.1 notanumber 1
mod/pkg/a.go:1.1,2.1 10 1
`)
	byPkg, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if c := byPkg["mod/pkg"]; c.total != 10 {
		t.Fatalf("total %d, want 10", c.total)
	}
}
