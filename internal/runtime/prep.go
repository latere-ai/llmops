// Package runtime turns a models/<name>.yaml manifest into a serving
// process: weight preparation, engine launch, and the latere health
// contract (specs/003-serving-runtime.md).
package runtime

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/mirror"
)

// PrepareWeights makes the model's weights available and returns the
// path (or URI) to hand to the engine.
//
//   - nvme-cache: sync the store prefix into cacheRoot/<repo>/<revision>,
//     verifying every file against _manifest.json. Idempotent — verified
//     files are not re-fetched; a flock serializes concurrent readers on
//     one host (specs/003).
//   - s3-stream: no staging; the engine streams directly from S3.
//   - local: the weights are already on disk. Verify in place and hand
//     the engine that directory — never copy it (specs/021).
func PrepareWeights(m *manifest.Manifest, cacheRoot string, store mirror.Store, log io.Writer) (string, error) {
	if m.Load == manifest.LoadS3Stream {
		return m.S3Prefix, nil
	}
	return prepareArtifact(m.Load, m.HFRepo, m.Revision, cacheRoot, store, log)
}

// PrepareDraft stages a speculator's draft head and returns the path to
// hand the engine. It returns "" when the head lives inside the target
// checkpoint, which is already prepared (specs/017).
//
// A draft head is an artifact like any other: same cache layout, same
// checksum verification, same pinned revision. That is the point of
// giving speculators their own hf_repo/revision rather than a flag —
// the freeze guarantee extends to them for free (specs/027).
func PrepareDraft(sp manifest.Speculator, load, cacheRoot string, store mirror.Store, log io.Writer) (string, error) {
	if !sp.SeparateHead() {
		return "", nil
	}
	// A draft checkpoint is opened from a path, so it is staged even when
	// the primary weights stream from S3.
	if load == manifest.LoadS3Stream {
		load = manifest.LoadNVMeCache
	}
	return prepareArtifact(load, sp.HFRepo, sp.Revision, cacheRoot, store, log)
}

// prepareArtifact stages one weight directory: verified in place for
// load: local, synced from the store otherwise.
func prepareArtifact(load, hfRepo, revision, cacheRoot string, store mirror.Store, log io.Writer) (string, error) {
	dir := filepath.Join(cacheRoot, hfRepo, revision)
	if load == manifest.LoadLocal {
		return verifyInPlace(dir, log)
	}
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
	_, _ = fmt.Fprintf(log, "weights: %d files ready in %s\n", len(sm.Files), dir)
	return dir, nil
}

// verifyInPlace checks an on-disk weight directory against its own
// _manifest.json and returns it unchanged (specs/021).
//
// Unlike nvme-cache this never re-fetches on a mismatch. There is no
// upstream behind a local store, so a bad hash is an operator problem,
// and silently repairing it would defeat the freeze the checksums exist
// to enforce. Copying is likewise avoided: the directory is already the
// artifact, and duplicating it would double the disk cost on a host
// that holds exactly one copy by design.
func verifyInPlace(dir string, log io.Writer) (string, error) {
	dir = filepath.Clean(dir)
	if fi, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("weights %s: %w", dir, err)
	} else if !fi.IsDir() {
		return "", fmt.Errorf("weights %s is not a directory", dir)
	}

	// Serialize against a mirror run writing this same directory.
	unlock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer unlock()

	sm, err := mirror.ReadManifest(mirror.OpenStore(dir))
	if err != nil {
		return "", fmt.Errorf("local store %s: %w", dir, err)
	}
	for _, f := range sm.Files {
		ok, err := fileMatches(filepath.Join(dir, f.Path), f)
		if err != nil {
			return "", fmt.Errorf("verify %s: %w", f.Path, err)
		}
		if !ok {
			return "", fmt.Errorf("verify %s: does not match %s; re-run `mirror freeze` on %s",
				f.Path, mirror.ManifestName, dir)
		}
	}
	_, _ = fmt.Fprintf(log, "weights: %d files verified in place in %s\n", len(sm.Files), dir)
	return dir, nil
}

// ensureFile makes one cache entry match its manifest hash, re-fetching
// once on corruption (specs/003 AC3).
func ensureFile(dir string, f mirror.FileEntry, store mirror.Store, log io.Writer) error {
	local := filepath.Join(dir, f.Path)
	if ok, _ := fileMatches(local, f); ok {
		return nil
	}
	_, _ = fmt.Fprintf(log, "weights: fetching %s (%d bytes)\n", f.Path, f.Size)
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
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing cached yet, which is a miss and not a failure.
		return false, nil
	}
	if err != nil {
		// An entry that is present but cannot be stat'd -- a permission
		// or a filesystem error -- was reported as a plain miss, so the
		// verification after the download reported "hash mismatch" for a
		// file it had never managed to look at.
		return false, fmt.Errorf("stat %s: %w", local, err)
	}
	if fi.Size() != f.Size {
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
		_ = f.Close()
		return nil, fmt.Errorf("flock %s: %w", dir, err)
	}
	return func() {
		// Closing the descriptor releases the lock regardless, so
		// neither call has an actionable failure.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
