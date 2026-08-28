// Package mirror freezes Hugging Face model weights into an object
// store: pull → verify → push → verify (specs/002-weights-registry.md).
package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ManifestName marks a completed mirror; it is written last so its
// presence is the atomicity signal (specs/002).
const ManifestName = "_manifest.json"

// StoreManifest is _manifest.json.
type StoreManifest struct {
	HFRepo     string      `json:"hf_repo"`
	Revision   string      `json:"revision"`
	MirroredAt string      `json:"mirrored_at"`
	TotalBytes int64       `json:"total_bytes"`
	Files      []FileEntry `json:"files"`
}

type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Commander runs external tools; tests substitute a fake.
type Commander interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecCommander shells out for real.
type ExecCommander struct{ Stdout, Stderr io.Writer }

func (e *ExecCommander) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = e.Stdout, e.Stderr
	return cmd.Run()
}

// Mirror wires the HF API, a downloader, and a Store.
type Mirror struct {
	HF  *HFClient
	Cmd Commander
	Log io.Writer
}

func (m *Mirror) logf(format string, a ...any) {
	if m.Log != nil {
		fmt.Fprintf(m.Log, format+"\n", a...)
	}
}

// Pull downloads repo@revision into dir and verifies it against the HF
// tree. Idempotent: verified local files are not re-downloaded.
func (m *Mirror) Pull(ctx context.Context, repo, revision, dir string) (sha string, files []FileEntry, err error) {
	sha, err = m.HF.Resolve(repo, revision)
	if err != nil {
		return "", nil, err
	}
	tree, err := m.HF.Tree(repo, sha)
	if err != nil {
		return "", nil, err
	}
	if err := CheckPolicy(tree); err != nil {
		return "", nil, err
	}
	if entries, err := verifyLocal(dir, tree); err == nil {
		m.logf("pull: %s@%s already verified locally, skipping download", repo, sha[:12])
		return sha, entries, nil
	}
	m.logf("pull: downloading %s@%s (%d files)", repo, sha[:12], len(tree))
	if err := m.Cmd.Run(ctx, "hf", "download", repo, "--revision", sha, "--local-dir", dir); err != nil {
		return "", nil, fmt.Errorf("hf download: %w", err)
	}
	entries, err := verifyLocal(dir, tree)
	if err != nil {
		return "", nil, fmt.Errorf("post-download verification: %w", err)
	}
	return sha, entries, nil
}

// verifyLocal checks every tree file exists with the right size, and the
// right SHA256 where HF publishes one (LFS); non-LFS files are hashed
// locally so the store manifest always carries a hash.
func verifyLocal(dir string, tree []TreeFile) ([]FileEntry, error) {
	var entries []FileEntry
	for _, f := range tree {
		p := filepath.Join(dir, f.Path)
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if fi.Size() != f.Size {
			return nil, fmt.Errorf("%s: size %d, want %d", f.Path, fi.Size(), f.Size)
		}
		sum, err := FileSHA256(p)
		if err != nil {
			return nil, err
		}
		if f.SHA256 != "" && sum != f.SHA256 {
			return nil, fmt.Errorf("%s: sha256 %s, want %s", f.Path, sum, f.SHA256)
		}
		entries = append(entries, FileEntry{Path: f.Path, Size: f.Size, SHA256: sum})
	}
	return entries, nil
}

// Push uploads dir to the store and writes _manifest.json last.
// Idempotent: files already present with matching size and content are
// skipped. Content is the identity, not size: Push is the repair path,
// so a size-only match would make a same-size-divergent object
// unreplaceable and leave the mirror permanently failing verify.
func (m *Mirror) Push(repo, sha, dir string, files []FileEntry, store Store) error {
	var total int64
	for _, f := range files {
		total += f.Size
		size, err := store.Size(f.Path)
		if err != nil {
			return err
		}
		if size == f.Size {
			sum, err := store.SHA256(f.Path)
			if err != nil {
				return err
			}
			if sum == f.SHA256 {
				m.logf("push: %s already present, skipping", f.Path)
				continue
			}
			m.logf("push: %s present but content differs, replacing", f.Path)
		}
		m.logf("push: %s (%d bytes)", f.Path, f.Size)
		if err := store.Put(filepath.Join(dir, f.Path), f.Path); err != nil {
			return err
		}
	}
	return writeManifest(repo, sha, total, files, store)
}

// Freeze writes _manifest.json into an already-populated directory,
// making it a store that can be served directly (specs/021).
//
// It is Push without the upload: the same HF tree verification and the
// same provenance artifact, for a directory that is already where the
// weights will be read from. A bare-metal host has no bucket to push
// to, but still needs the pinned revision and per-file checksums that
// make the freeze guarantee mean something.
func (m *Mirror) Freeze(repo, revision, dir string) (string, error) {
	sha, err := m.HF.Resolve(repo, revision)
	if err != nil {
		return "", err
	}
	tree, err := m.HF.Tree(repo, sha)
	if err != nil {
		return "", err
	}
	if err := CheckPolicy(tree); err != nil {
		return "", err
	}
	files, err := verifyLocal(dir, tree)
	if err != nil {
		return "", fmt.Errorf("verify %s: %w", dir, err)
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if err := writeManifest(repo, sha, total, files, &LocalStore{Root: dir}); err != nil {
		return "", err
	}
	m.logf("freeze: %d files, %d bytes, %s written to %s", len(files), total, ManifestName, dir)
	return sha, nil
}

// writeManifest writes _manifest.json into a store. It is written last
// so a partial mirror stays detectable: the manifest's presence is what
// marks the store complete.
func writeManifest(repo, sha string, total int64, files []FileEntry, store Store) error {
	sm := StoreManifest{
		HFRepo:     repo,
		Revision:   sha,
		MirroredAt: time.Now().UTC().Format(time.RFC3339),
		TotalBytes: total,
		Files:      files,
	}
	data, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return store.Put(tmp.Name(), ManifestName)
}

// ReadManifest fetches and parses _manifest.json from a store.
func ReadManifest(store Store) (*StoreManifest, error) {
	tmp, err := os.CreateTemp("", "manifest-*")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := store.Get(ManifestName, tmp.Name()); err != nil {
		return nil, fmt.Errorf("mirror incomplete or absent (%s missing): %w", ManifestName, err)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	var sm StoreManifest
	if err := json.Unmarshal(data, &sm); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	return &sm, nil
}

// Verify re-hashes every file in the store against _manifest.json.
func (m *Mirror) Verify(store Store) error {
	sm, err := ReadManifest(store)
	if err != nil {
		return err
	}
	for _, f := range sm.Files {
		size, err := store.Size(f.Path)
		if err != nil {
			return err
		}
		if size != f.Size {
			return fmt.Errorf("verify %s: size %d, want %d", f.Path, size, f.Size)
		}
		sum, err := store.SHA256(f.Path)
		if err != nil {
			return err
		}
		if sum != f.SHA256 {
			return fmt.Errorf("verify %s: sha256 %s, want %s", f.Path, sum, f.SHA256)
		}
		m.logf("verify: %s ok", f.Path)
	}
	m.logf("verify: %d files, %d bytes, all match", len(sm.Files), sm.TotalBytes)
	return nil
}
