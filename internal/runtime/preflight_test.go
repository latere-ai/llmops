package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/llmops/internal/manifest"
)

// fakeMeminfo points the preflight at a synthetic pool so the test runs
// the same on a laptop and on the box.
func fakeMeminfo(t *testing.T, totalGiB int) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meminfo")
	body := fmt.Sprintf("MemFree:         1000 kB\nMemTotal:       %d kB\nMemAvailable:    2000 kB\n",
		int64(totalGiB)*1024*1024)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := meminfoPath
	meminfoPath = p
	t.Cleanup(func() { meminfoPath = old })
}

// weightsOfSize writes a directory holding roughly gib gibibytes, as a
// sparse file so the test costs no disk.
func weightsOfSize(t *testing.T, gibSize int) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(gibSize) << 30); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gb10Manifest(frac string) *manifest.Manifest {
	return &manifest.Manifest{
		Name:       "qwen3.8-27b-fast",
		HFRepo:     "acme/tiny",
		Revision:   sha,
		Format:     "nvfp4",
		License:    "apache-2.0",
		Runtime:    manifest.RuntimeSGLang,
		Load:       manifest.LoadLocal,
		GPU:        manifest.GPU{Type: manifest.GPUTypeGB10, Count: 1, Nodes: 1},
		ContextMax: 262144,
		Args:       []string{"--context-length=262144", "--mem-fraction-static=" + frac},
	}
}

// TestPreflightRefusesTheStartThatTookTheHostDown reproduces the
// 2026-08-29 incident in arithmetic: a 128 GB unified pool, a manifest
// at the 0.80 ceiling, and a 23 GB checkpoint just written to disk.
//
// 102.4 + 23 + 8 = 133.4 GiB of a 128 GiB pool. The kernel was left
// about 2.6 GB, could not reclaim device memory, and stalled — which on
// a box with no out-of-band access costs the machine, not the service.
func TestPreflightRefusesTheStartThatTookTheHostDown(t *testing.T) {
	fakeMeminfo(t, 128)
	dir := weightsOfSize(t, 23)

	err := CheckMemoryBudget(gb10Manifest("0.80"), dir, io.Discard)
	if err == nil {
		t.Fatal("the configuration that took the host down was allowed to start")
	}
	for _, want := range []string{"refusing to start", "--mem-fraction-static"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// It must say what would work; an error that only says no gets
	// worked around by raising something else.
	if !strings.Contains(err.Error(), "0.7") {
		t.Errorf("error suggests no workable fraction: %v", err)
	}
}

// TestPreflightAllowsTheFractionThatServes: 0.65 is what the BF16
// endpoint ran at for hours on this box, so the guard must not block it.
func TestPreflightAllowsTheFractionThatServes(t *testing.T) {
	fakeMeminfo(t, 128)
	dir := weightsOfSize(t, 23)

	if err := CheckMemoryBudget(gb10Manifest("0.65"), dir, io.Discard); err != nil {
		t.Fatalf("0.65 with a 23 GiB checkpoint was refused: %v", err)
	}
}

// TestPreflightCountsTheCheckpoint is the term a static ceiling misses.
// The same fraction is safe for a small model and fatal for a large one,
// because the weights sit in page cache exactly when memory is tightest.
func TestPreflightCountsTheCheckpoint(t *testing.T) {
	fakeMeminfo(t, 128)
	m := gb10Manifest("0.75")

	if err := CheckMemoryBudget(m, weightsOfSize(t, 5), io.Discard); err != nil {
		t.Fatalf("0.75 with a 5 GiB checkpoint was refused: %v", err)
	}
	if err := CheckMemoryBudget(m, weightsOfSize(t, 40), io.Discard); err == nil {
		t.Fatal("0.75 with a 40 GiB checkpoint was allowed")
	}
}

// TestPreflightSkipsDiscreteHardware: on a discrete-HBM node an
// over-allocation kills the engine and the host survives, which is the
// normal OOM contract. The guard is specific to a shared pool.
func TestPreflightSkipsDiscreteHardware(t *testing.T) {
	fakeMeminfo(t, 128)
	m := gb10Manifest("0.80")
	m.GPU = manifest.GPU{Type: "h200", Count: 8, Nodes: 1}

	if err := CheckMemoryBudget(m, weightsOfSize(t, 60), io.Discard); err != nil {
		t.Fatalf("discrete hardware was subjected to the unified-memory rule: %v", err)
	}
}

// TestPreflightSkipsWhatItCannotDetermine: a guard that blocks a good
// start on missing information gets switched off, and then protects
// nothing.
func TestPreflightSkipsWhatItCannotDetermine(t *testing.T) {
	dir := weightsOfSize(t, 23)

	// No /proc/meminfo — a laptop, or a kernel that reports elsewhere.
	old := meminfoPath
	meminfoPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { meminfoPath = old })
	var log strings.Builder
	if err := CheckMemoryBudget(gb10Manifest("0.80"), dir, &log); err != nil {
		t.Fatalf("unreadable meminfo blocked a start: %v", err)
	}
	if !strings.Contains(log.String(), "skipping") {
		t.Errorf("the skip was silent: %s", log.String())
	}

	// An unreadable weights directory.
	meminfoPath = old
	fakeMeminfo(t, 128)
	if err := CheckMemoryBudget(gb10Manifest("0.80"), filepath.Join(dir, "nope"), io.Discard); err != nil {
		t.Fatalf("an unsizeable checkpoint blocked a start: %v", err)
	}

	// A manifest with no fraction: validation already rejects that, and
	// duplicating the rule here would report it twice.
	m := gb10Manifest("0.80")
	m.Args = []string{"--context-length=4096"}
	if err := CheckMemoryBudget(m, dir, io.Discard); err != nil {
		t.Fatalf("a missing fraction was reported by the preflight: %v", err)
	}
	m.Args = []string{"--mem-fraction-static=banana"}
	if err := CheckMemoryBudget(m, dir, io.Discard); err != nil {
		t.Fatalf("an unparseable fraction was reported by the preflight: %v", err)
	}
}

func TestFloorTo2dp(t *testing.T) {
	// Rounding down matters: a suggestion that rounds up would name a
	// fraction the same check then refuses.
	cases := map[float64]float64{0.7289: 0.72, 0.65: 0.65, -0.1: 0}
	for in, want := range cases {
		if got := floorTo2dp(in); got != want {
			t.Errorf("floorTo2dp(%v) = %v, want %v", in, got, want)
		}
	}
}
