package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/latere-ai/llmops/internal/deploycheck"
	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/runtime"
)

// runServing handles the two verbs that act on a manifest: starting the
// model it describes, and checking it describes something coherent
// (specs/003).
func runServing(cmd string, rest []string, out, errw io.Writer) error {
	switch cmd {
	case "validate":
		fs := flag.NewFlagSet("validate", flag.ContinueOnError)
		fs.SetOutput(errw)
		deployDir := fs.String("deploy", "", "deploy directory (default: <models-dir>/../deploy)")
		target, flags := popPositional(rest)
		if err := fs.Parse(flags); err != nil || target == "" {
			return usagef("usage: llmops validate <models-dir | manifest.yaml> [--deploy <dir>]")
		}
		return validate(target, *deployDir, out)

	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(errw)
		path := fs.String("manifest", "", "path to models/<name>.yaml")
		port := fs.Int("port", 8000, "shim listen port")
		enginePort := fs.Int("engine-port", 30000, "engine listen port")
		cacheRoot := fs.String("cache-root", "/cache", "weights root on this host")
		speculator := fs.String("speculator", "",
			`draft-model configuration to serve with; "none" disables speculation (default: the manifest's)`)
		if err := fs.Parse(rest); err != nil || *path == "" {
			return usagef("usage: llmops serve --manifest <manifest.yaml> [--speculator <name|none>]")
		}
		m, err := manifest.Load(*path)
		if err != nil {
			return err
		}
		opts := runtime.Options{
			Port:       *port,
			EnginePort: *enginePort,
			CacheRoot:  *cacheRoot,
			Speculator: *speculator,
			Log:        errw,
		}
		// Test/debug hook: replace the engine command.
		if o := os.Getenv("LLMOPS_ENGINE_CMD"); o != "" {
			opts.EngineCmd = strings.Fields(o)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return runtime.Serve(ctx, m, opts)
	}
	return fmt.Errorf("unknown serving command %q", cmd)
}

// validate checks manifests, and for a directory also checks each
// model's deploy artifact agrees with it.
//
// The deploy half used to run only from deploycheck's own test, so a
// deploy file could contradict its manifest and nothing outside `go
// test` would say so. Running it here is what makes the consistency
// guarantee real for anyone holding the binary.
func validate(path, deployDir string, out io.Writer) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		m, err := manifest.Load(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: ok\n", m.Name)
		return nil
	}

	ms, err := manifest.LoadDir(path)
	if err != nil {
		return err
	}
	for _, m := range ms {
		fmt.Fprintf(out, "%s: ok (%s, %s, %s, %dx%s x%d)\n",
			m.Name, m.Runtime, m.DeployMode(), m.Format, m.GPU.Count, m.GPU.Type, m.GPU.Nodes)
	}
	fmt.Fprintf(out, "%d manifests valid\n", len(ms))

	if deployDir == "" {
		deployDir = filepath.Join(filepath.Dir(filepath.Clean(path)), "deploy")
	}
	if _, err := os.Stat(deployDir); err != nil {
		// Say so rather than passing quietly: a silent skip is how the
		// deploy check went unrun in the first place.
		return fmt.Errorf("deploy directory %s: %w (pass --deploy to point elsewhere)", deployDir, err)
	}
	if err := deploycheck.Validate(path, deployDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "%d deploy artifacts consistent\n", len(ms))
	return nil
}
