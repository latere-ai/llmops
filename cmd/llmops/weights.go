package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/latere-ai/llmops/internal/mirror"
)

// runWeights handles the five verbs that act on a weight store
// (specs/002, specs/021).
func runWeights(cmd string, rest []string, out, errw io.Writer) error {
	switch cmd {
	case "pull":
		fs := flag.NewFlagSet("pull", flag.ContinueOnError)
		fs.SetOutput(errw)
		dir := fs.String("dir", "", "local weights directory")
		target, flags := popPositional(rest)
		if err := fs.Parse(flags); err != nil || target == "" || *dir == "" {
			return usagef("usage: llmops pull <hf_repo>[@revision] --dir <dir>")
		}
		repo, rev := splitRepo(target)
		sha, files, err := newMirror(errw).Pull(context.Background(), repo, rev, *dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s@%s: %d files verified in %s\n", repo, sha, len(files), *dir)
		return nil

	case "freeze":
		fs := flag.NewFlagSet("freeze", flag.ContinueOnError)
		fs.SetOutput(errw)
		dir := fs.String("dir", "", "weights directory to freeze in place")
		target, flags := popPositional(rest)
		if err := fs.Parse(flags); err != nil || target == "" || *dir == "" {
			return usagef("usage: llmops freeze <hf_repo>@<sha> --dir <weights-dir>")
		}
		repo, rev := splitRepo(target)
		if rev == "" {
			return fmt.Errorf("freeze requires a pinned revision: <hf_repo>@<sha>")
		}
		sha, err := newMirror(errw).Freeze(repo, rev, *dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "froze %s@%s in %s\n", repo, sha, *dir)
		return nil

	case "push":
		fs := flag.NewFlagSet("push", flag.ContinueOnError)
		fs.SetOutput(errw)
		dir := fs.String("dir", "", "local scratch directory")
		bucket := fs.String("bucket", "", "store root (s3://bucket or local path)")
		target, flags := popPositional(rest)
		if err := fs.Parse(flags); err != nil || target == "" || *dir == "" || *bucket == "" {
			return usagef("usage: llmops push <hf_repo>@<sha> --dir <scratch> --bucket <root>")
		}
		repo, rev := splitRepo(target)
		if rev == "" {
			return fmt.Errorf("push requires a pinned revision: <hf_repo>@<sha>")
		}
		m := newMirror(errw)
		// Pull is idempotent: with the scratch dir already verified it
		// only re-checks hashes, guaranteeing we push exactly what HF
		// advertises.
		sha, files, err := m.Pull(context.Background(), repo, rev, *dir)
		if err != nil {
			return err
		}
		prefix := strings.TrimSuffix(*bucket, "/") + "/" + repo + "/" + sha + "/"
		if err := m.Push(repo, sha, *dir, files, mirror.OpenStore(prefix)); err != nil {
			return err
		}
		fmt.Fprintf(out, "pushed %s@%s to %s\n", repo, sha, prefix)
		return nil

	case "verify":
		if len(rest) != 1 {
			return usagef("usage: llmops verify <prefix>")
		}
		if err := newMirror(errw).Verify(mirror.OpenStore(rest[0])); err != nil {
			return err
		}
		fmt.Fprintf(out, "verify %s: ok\n", rest[0])
		return nil

	case "list":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		fs.SetOutput(errw)
		bucket := fs.String("bucket", "", "store root (s3://bucket or local path)")
		if err := fs.Parse(rest); err != nil || *bucket == "" {
			return usagef("usage: llmops list --bucket <root>")
		}
		files, err := mirror.OpenStore(*bucket).List()
		if err != nil {
			return err
		}
		for _, f := range files {
			if before, ok := strings.CutSuffix(f, mirror.ManifestName); ok {
				fmt.Fprintln(out, before)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown weights command %q", cmd)
}
