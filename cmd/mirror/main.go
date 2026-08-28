// Command mirror freezes Hugging Face model weights into an object
// store (specs/002-weights-registry.md).
//
//	mirror pull <hf_repo>[@revision] --dir <scratch>
//	mirror push <hf_repo>@<sha> --dir <scratch> --bucket <s3://bucket | path>
//	mirror verify <prefix>
//	mirror ls --bucket <s3://bucket | path>
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/latere-ai/llmops/internal/mirror"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func newMirror(errw io.Writer) *mirror.Mirror {
	hf := mirror.NewHFClient()
	if base := os.Getenv("OPENLLMS_HF_BASE"); base != "" {
		hf.Base = base
	}
	return &mirror.Mirror{
		HF:  hf,
		Cmd: &mirror.ExecCommander{Stdout: errw, Stderr: errw},
		Log: errw,
	}
}

func splitRepo(arg string) (repo, revision string) {
	repo, revision, _ = strings.Cut(arg, "@")
	return repo, revision
}

// popPositional splits a leading non-flag argument (Go's flag package
// stops at the first positional, so `pull repo --dir X` needs it popped
// before Parse).
func popPositional(rest []string) (pos string, flags []string) {
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		return rest[0], rest[1:]
	}
	return "", rest
}

func run(args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errw, "usage: mirror <pull|push|freeze|verify|ls> [args]")
		return 2
	}
	cmd, rest := args[0], args[1:]
	err := func() error {
		switch cmd {
		case "pull":
			fs := flag.NewFlagSet("pull", flag.ContinueOnError)
			fs.SetOutput(errw)
			dir := fs.String("dir", "", "local scratch directory")
			target, flags := popPositional(rest)
			if err := fs.Parse(flags); err != nil || target == "" || *dir == "" {
				return fmt.Errorf("usage: mirror pull <hf_repo>[@revision] --dir <scratch>")
			}
			repo, rev := splitRepo(target)
			sha, files, err := newMirror(errw).Pull(context.Background(), repo, rev, *dir)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s@%s: %d files verified in %s\n", repo, sha, len(files), *dir)
			return nil
		case "push":
			fs := flag.NewFlagSet("push", flag.ContinueOnError)
			fs.SetOutput(errw)
			dir := fs.String("dir", "", "local scratch directory")
			bucket := fs.String("bucket", "", "store root (s3://bucket or local path)")
			target, flags := popPositional(rest)
			if err := fs.Parse(flags); err != nil || target == "" || *dir == "" || *bucket == "" {
				return fmt.Errorf("usage: mirror push <hf_repo>@<sha> --dir <scratch> --bucket <root>")
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
			store := mirror.OpenStore(prefix)
			if err := m.Push(repo, sha, *dir, files, store); err != nil {
				return err
			}
			fmt.Fprintf(out, "pushed %s@%s to %s\n", repo, sha, prefix)
			return nil
		case "freeze":
			fs := flag.NewFlagSet("freeze", flag.ContinueOnError)
			fs.SetOutput(errw)
			dir := fs.String("dir", "", "weights directory to freeze in place")
			target, flags := popPositional(rest)
			if err := fs.Parse(flags); err != nil || target == "" || *dir == "" {
				return fmt.Errorf("usage: mirror freeze <hf_repo>@<sha> --dir <weights-dir>")
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
		case "verify":
			if len(rest) != 1 {
				return fmt.Errorf("usage: mirror verify <prefix>")
			}
			if err := newMirror(errw).Verify(mirror.OpenStore(rest[0])); err != nil {
				return err
			}
			fmt.Fprintf(out, "verify %s: ok\n", rest[0])
			return nil
		case "ls":
			fs := flag.NewFlagSet("ls", flag.ContinueOnError)
			fs.SetOutput(errw)
			bucket := fs.String("bucket", "", "store root (s3://bucket or local path)")
			if err := fs.Parse(rest); err != nil || *bucket == "" {
				return fmt.Errorf("usage: mirror ls --bucket <root>")
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
		default:
			return fmt.Errorf("unknown command %q", cmd)
		}
	}()
	if err != nil {
		fmt.Fprintln(errw, "mirror:", err)
		return 1
	}
	return 0
}
