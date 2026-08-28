package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/runtime"
)

// runServing handles the two verbs that act on a manifest: starting the
// model it describes, and checking it describes something coherent
// (specs/003).
func runServing(cmd string, rest []string, out, errw io.Writer) error {
	switch cmd {
	case "validate":
		if len(rest) != 1 {
			return usagef("usage: llmops validate <models-dir | manifest.yaml>")
		}
		return validate(rest[0], out)

	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(errw)
		path := fs.String("manifest", "", "path to models/<name>.yaml")
		port := fs.Int("port", 8000, "shim listen port")
		enginePort := fs.Int("engine-port", 30000, "engine listen port")
		cacheRoot := fs.String("cache-root", "/cache", "weights root on this host")
		if err := fs.Parse(rest); err != nil || *path == "" {
			return usagef("usage: llmops serve --manifest <manifest.yaml>")
		}
		m, err := manifest.Load(*path)
		if err != nil {
			return err
		}
		opts := runtime.Options{
			Port:       *port,
			EnginePort: *enginePort,
			CacheRoot:  *cacheRoot,
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

func validate(path string, out io.Writer) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		ms, err := manifest.LoadDir(path)
		if err != nil {
			return err
		}
		for _, m := range ms {
			fmt.Fprintf(out, "%s: ok (%s, %s, %dx%s x%d)\n",
				m.Name, m.Runtime, m.Format, m.GPU.Count, m.GPU.Type, m.GPU.Nodes)
		}
		fmt.Fprintf(out, "%d manifests valid\n", len(ms))
		return nil
	}
	m, err := manifest.Load(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: ok\n", m.Name)
	return nil
}
