package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Store is an object store rooted at a prefix. Remote paths are
// prefix-relative. LocalStore backs tests and file:// e2e; S5cmdStore
// shells to s5cmd for real S3 (specs/002).
type Store interface {
	Put(local, remote string) error
	Get(remote, local string) error
	List() ([]string, error)
	Size(remote string) (int64, error) // -1 if absent
	SHA256(remote string) (string, error)
}

// OpenStore picks a Store from a prefix: s3://… → s5cmd, anything else
// (file:// or a plain path) → local filesystem.
func OpenStore(prefix string) Store {
	if strings.HasPrefix(prefix, "s3://") {
		return &S5cmdStore{Prefix: strings.TrimSuffix(prefix, "/") + "/"}
	}
	return &LocalStore{Root: strings.TrimPrefix(prefix, "file://")}
}

// LocalStore is a directory-rooted Store.
type LocalStore struct{ Root string }

func (s *LocalStore) path(remote string) string { return filepath.Join(s.Root, remote) }

func (s *LocalStore) Put(local, remote string) error {
	dst := s.path(remote)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(local)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}

func (s *LocalStore) Get(remote, local string) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	in, err := os.Open(s.path(remote))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(local)
	if err != nil {
		return err
	}
	// Close reports the flush error, so it is returned rather than
	// deferred: a discarded flush lets Get succeed on a short file.
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (s *LocalStore) List() ([]string, error) {
	var out []string
	err := filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".put-") {
			return err
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

func (s *LocalStore) Size(remote string) (int64, error) {
	fi, err := os.Stat(s.path(remote))
	if os.IsNotExist(err) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	return fi.Size(), nil
}

func (s *LocalStore) SHA256(remote string) (string, error) {
	return FileSHA256(s.path(remote))
}

// FileSHA256 hashes a local file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// S5cmdStore shells to s5cmd for S3. Thin by design; correctness is
// carried by the same Mirror logic the LocalStore e2e covers.
type S5cmdStore struct{ Prefix string }

type s5cmdExitError struct {
	command string
	cause   error
	stderr  []byte
}

func (e *s5cmdExitError) Error() string {
	return fmt.Sprintf("s5cmd %s: %v: %s", e.command, e.cause, e.stderr)
}

func (e *s5cmdExitError) Unwrap() error { return e.cause }

func (s *S5cmdStore) run(args ...string) ([]byte, error) {
	// The Store interface carries no context, so there is none to inherit
	// here. s5cmd exits on its own; nothing in the process waits on a
	// deadline this could honour.
	out, err := exec.CommandContext(context.Background(), "s5cmd", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, &s5cmdExitError{
				command: strings.Join(args, " "),
				cause:   err,
				stderr:  bytes.TrimSpace(ee.Stderr),
			}
		}
		return nil, fmt.Errorf("s5cmd %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (s *S5cmdStore) Put(local, remote string) error {
	_, err := s.run("cp", local, s.Prefix+remote)
	return err
}

func (s *S5cmdStore) Get(remote, local string) error {
	_, err := s.run("cp", s.Prefix+remote, local)
	return err
}

func (s *S5cmdStore) List() ([]string, error) {
	out, err := s.run("ls", s.Prefix+"*")
	if err != nil {
		return nil, err
	}
	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			files = append(files, strings.TrimPrefix(fields[len(fields)-1], s.Prefix))
		}
	}
	return files, nil
}

func (s *S5cmdStore) Size(remote string) (int64, error) {
	out, err := s.run("ls", s.Prefix+remote)
	if err != nil {
		var exitErr *s5cmdExitError
		if errors.As(err, &exitErr) && strings.HasSuffix(string(exitErr.stderr), ": no object found") {
			return -1, nil
		}
		return -1, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 3 {
		return -1, fmt.Errorf("unexpected s5cmd ls output: %q", out)
	}
	var n int64
	if _, err := fmt.Sscan(fields[2], &n); err != nil {
		return -1, err
	}
	return n, nil
}

// SHA256 downloads to a temp file and hashes — no server-side hash
// assumption (S3 additional-checksum support varies by uploader).
func (s *S5cmdStore) SHA256(remote string) (string, error) {
	tmp, err := os.CreateTemp("", "mirror-hash-*")
	if err != nil {
		return "", err
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := s.Get(remote, tmp.Name()); err != nil {
		return "", err
	}
	return FileSHA256(tmp.Name())
}
