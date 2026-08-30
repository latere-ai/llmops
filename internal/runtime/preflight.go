package runtime

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/latere-ai/llmops/internal/manifest"
)

// hostFloorBytes is the memory a unified-memory host keeps for itself
// beyond the checkpoint it is loading: kernel, sshd, the tailnet daemon,
// and whatever desktop session this class of box tends to have running.
//
// 8 GB is deliberately blunt. The point is not to model the host's
// usage precisely, it is to keep the last few gigabytes from being the
// difference between a service dying and a machine dying.
const hostFloorBytes = 8 << 30

// meminfoPath is the kernel's memory report, overridable for tests.
var meminfoPath = "/proc/meminfo"

// CheckMemoryBudget refuses a start that cannot leave the host enough
// memory to stay alive.
//
// This exists because of a specific incident (2026-08-29). On gb10 the
// CPU and GPU share one 128 GB pool and the engine fills whatever
// fraction it is given, so a manifest at 0.80 claimed 102.4 GB. The
// 23 GB checkpoint had been written 31 seconds earlier and was still in
// page cache. The kernel was left about 2.6 GB, could not reclaim
// device memory, and stalled — taking SSH with it, on a box with no
// out-of-band access.
//
// The rule is the arithmetic that incident produced:
//
//	engine_fraction x MemTotal  +  checkpoint_bytes  +  host_floor  <=  MemTotal
//
// The checkpoint term is the part a static ceiling misses. Weights are
// read immediately before the engine's largest allocation, so they are
// in page cache exactly when memory is tightest. Counting them as free
// because they are reclaimable is what makes this failure survivable in
// theory and fatal in practice: reclaim has to complete faster than the
// allocator demands, and here it does not.
//
// Anything that cannot be determined is skipped rather than guessed —
// a preflight that blocks a good start is worse than one that misses a
// bad one, because it gets disabled.
func CheckMemoryBudget(m *manifest.Manifest, weightsDir string, log io.Writer) error {
	if m.GPU.Type != manifest.GPUTypeGB10 {
		return nil // discrete HBM: an over-allocation kills the engine, not the host
	}
	total, err := memTotalBytes()
	if err != nil {
		_, _ = fmt.Fprintf(log, "preflight: cannot read %s (%v); skipping the memory budget check\n", meminfoPath, err)
		return nil
	}
	frac, ok := engineFraction(m)
	if !ok {
		return nil // validation already requires the flag; nothing to check against
	}
	ckpt, err := dirBytes(weightsDir)
	if err != nil {
		_, _ = fmt.Fprintf(log, "preflight: cannot size %s (%v); skipping the memory budget check\n", weightsDir, err)
		return nil
	}

	engine := int64(frac * float64(total))
	need := engine + ckpt + hostFloorBytes
	_, _ = fmt.Fprintf(log, "preflight: pool %s, engine %s (%.2f), checkpoint %s, host floor %s\n",
		gib(total), gib(engine), frac, gib(ckpt), gib(hostFloorBytes))
	if need <= total {
		return nil
	}

	// Say what would work, not just that this does not.
	safe := float64(total-ckpt-hostFloorBytes) / float64(total)
	return fmt.Errorf(
		"refusing to start: %s + checkpoint %s + host floor %s = %s, more than this host's %s.\n"+
			"  On %s the CPU and GPU share one pool and the engine fills its fraction, so this "+
			"leaves the kernel too little to stay reachable — it stalls rather than killing the engine.\n"+
			"  Lower %s to %.2f or below",
		gib(engine), gib(ckpt), gib(hostFloorBytes), gib(need), gib(total),
		manifest.GPUTypeGB10, memFractionFlagFor(m), floorTo2dp(safe))
}

// engineFraction reads the memory fraction the manifest states.
func engineFraction(m *manifest.Manifest) (float64, bool) {
	v, ok := m.FlagValue(memFractionFlagFor(m))
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return f, true
}

func memFractionFlagFor(m *manifest.Manifest) string {
	if m.Runtime == manifest.RuntimeSGLang {
		return "--mem-fraction-static"
	}
	return "--gpu-memory-utilization"
}

// memTotalBytes reads MemTotal, the size of the pool both processors
// draw from on this class.
func memTotalBytes() (int64, error) {
	data, err := os.ReadFile(meminfoPath)
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb << 10, nil
	}
	return 0, fmt.Errorf("no MemTotal line")
}

// dirBytes totals the regular files in a weight directory.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// floorTo2dp rounds down, so the suggestion is never a value that would
// itself be refused.
func floorTo2dp(f float64) float64 {
	if f < 0 {
		return 0
	}
	return float64(int(f*100)) / 100
}
