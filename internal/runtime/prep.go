// Package runtime turns a models/<name>.yaml manifest into a serving
// process: weight preparation, engine launch, and the latere health
// contract (specs/003-serving-runtime.md).
package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/latere-ai/open-llms/internal/manifest"
	"github.com/latere-ai/open-llms/internal/mirror"
)

// PrepareWeights makes the model's weights available and returns the
// path (or URI) to hand to the engine.
//
//   - nvme-cache: sync the store prefix into cacheRoot/<repo>/<revision>,
//     verifying every file against _manifest.json. Idempotent — verified
//     files are not re-fetched; a same-node flock serializes concurrent
//     pods (specs/003).
//   - s3-stream: no staging; the engine streams directly from S3.
func PrepareWeights(m *manifest.Manifest, cacheRoot string, store mirror.Store, log io.Writer) (string, error) {
	if m.Load == manifest.LoadS3Stream {
		return m.S3Prefix, nil
	}
	dir := filepath.Join(cacheRoot, m.HFRepo, m.Revision)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer unlock()

	sm, err := mirror.ReadManifest(store)
	if err != nil {
		return "", err
	}
	for _, f := range sm.Files {
		if err := ensureFile(dir, f, store, log); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(log, "weights: %d files ready in %s\n", len(sm.Files), dir)
	return dir, nil
}

// ensureFile makes one cache entry match its manifest hash, re-fetching
// once on corruption (specs/003 AC3).
func ensureFile(dir string, f mirror.FileEntry, store mirror.Store, log io.Writer) error {
	local := filepath.Join(dir, f.Path)
	if ok, _ := fileMatches(local, f); ok {
		return nil
	}
	fmt.Fprintf(log, "weights: fetching %s (%d bytes)\n", f.Path, f.Size)
	if err := store.Get(f.Path, local); err != nil {
		return fmt.Errorf("fetch %s: %w", f.Path, err)
	}
	ok, err := fileMatches(local, f)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fetch %s: hash mismatch after download", f.Path)
	}
	return nil
}

func fileMatches(local string, f mirror.FileEntry) (bool, error) {
	fi, err := os.Stat(local)
	if err != nil || fi.Size() != f.Size {
		return false, nil
	}
	sum, err := mirror.FileSHA256(local)
	if err != nil {
		return false, err
	}
	return sum == f.SHA256, nil
}

// lockDir takes an exclusive flock on <dir>/.lock so concurrent pods on
// one node share the cache without racing.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", dir, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
