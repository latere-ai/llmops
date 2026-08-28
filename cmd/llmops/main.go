// Command llmops owns the open-weights inference layer end to end:
// fetching and freezing weights, serving a model, and measuring what is
// serving (specs/024-single-cli.md).
//
//	llmops pull     <hf_repo>[@revision] --dir <dir>
//	llmops freeze   <hf_repo>@<sha> --dir <dir>
//	llmops push     <hf_repo>@<sha> --dir <dir> --bucket <s3://bucket | path>
//	llmops verify   <prefix>
//	llmops list     --bucket <s3://bucket | path>
//	llmops serve    --manifest <manifest.yaml>
//	llmops validate <models-dir | manifest.yaml>
//	llmops bench    --url <base> --model <id>
//	llmops version
//
// The verbs are flat. Grouping them under an object would name an
// implementation rather than an intent, and `pull` is the most-typed of
// them — see specs/024.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/latere-ai/llmops/internal/mirror"
)

// version and commit are injected at build time by `make dist`. Left
// empty, they fall back to whatever the Go toolchain stamped, so a
// `go build` binary still answers `llmops version` honestly.
var (
	version = ""
	commit  = ""
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: llmops <command> [args]

weights
  pull     <hf_repo>[@revision] --dir <dir>    fetch from Hugging Face
  freeze   <hf_repo>@<sha> --dir <dir>         write the store manifest in place
  push     <hf_repo>@<sha> --dir <dir> --bucket <root>
  verify   <prefix>                            check a store against its manifest
  list     --bucket <root>                     what is mirrored there

serving
  serve    --manifest <manifest.yaml>          run a model
  validate <models-dir | manifest.yaml>        check manifests and deploys
  bench    --url <base> --model <id>           measure a live endpoint

  version                                      build version and commit`

// usageErr marks a caller mistake — an unknown command, a missing
// required flag, an unparseable one. It exits 2 rather than 1 so a
// script can tell "you invoked this wrong" from "the work failed".
//
// The three binaries this replaced disagreed on that: bench exited 2 on
// a bad flag while mirror and runtime exited 1. One command needs one
// convention, so the stricter of the two wins.
type usageErr struct{ error }

func usagef(format string, a ...any) error {
	return usageErr{fmt.Errorf(format, a...)}
}

func run(args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errw, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	err := func() error {
		switch cmd {
		case "pull", "freeze", "push", "verify", "list":
			return runWeights(cmd, rest, out, errw)
		case "serve", "validate":
			return runServing(cmd, rest, out, errw)
		case "bench":
			return runBench(rest, out, errw)
		case "version":
			fmt.Fprintln(out, versionString())
			return nil
		default:
			fmt.Fprintln(errw, usage)
			return usagef("unknown command %q", cmd)
		}
	}()
	if err != nil {
		fmt.Fprintln(errw, "llmops:", err)
		if errors.As(err, &usageErr{}) {
			return 2
		}
		return 1
	}
	return 0
}

// versionString prefers the values stamped by `make dist` and falls back
// to the module's build info, so a binary built any way can say what it
// is. A hand-copied binary has no image tag to ask (specs/020).
func versionString() string {
	v, c := version, commit
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "" {
			v = info.Main.Version
		}
		if c == "" {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" {
					c = s.Value
				}
			}
		}
	}
	if v == "" {
		v = "(devel)"
	}
	if c == "" {
		return v
	}
	if len(c) > 12 {
		c = c[:12]
	}
	return v + " (" + c + ")"
}

// dispatch is the set of verbs `run` accepts, kept beside it so the
// usage text and the switch cannot drift apart.
var dispatch = []string{
	"pull", "freeze", "push", "verify", "list",
	"serve", "validate", "bench", "version",
}

func newMirror(errw io.Writer) *mirror.Mirror {
	hf := mirror.NewHFClient()
	if base := os.Getenv("LLMOPS_HF_BASE"); base != "" {
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
