// Command runtime is the serving-container entrypoint
// (specs/003-serving-runtime.md).
//
//	runtime validate <models-dir | manifest.yaml>
//	runtime serve --manifest <manifest.yaml> [--port 8000]
//	               [--engine-port 30000] [--cache-root /cache]
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errw, "usage: runtime <validate|serve> [args]")
		return 2
	}
	err := func() error {
		switch args[0] {
		case "validate":
			if len(args) != 2 {
				return fmt.Errorf("usage: runtime validate <models-dir | manifest.yaml>")
			}
			return validate(args[1], out)
		case "serve":
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			fs.SetOutput(errw)
			path := fs.String("manifest", "", "path to models/<name>.yaml")
			port := fs.Int("port", 8000, "shim listen port")
			enginePort := fs.Int("engine-port", 30000, "engine listen port")
			cacheRoot := fs.String("cache-root", "/cache", "NVMe cache root")
			if err := fs.Parse(args[1:]); err != nil || *path == "" {
				return fmt.Errorf("usage: runtime serve --manifest <manifest.yaml>")
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
		default:
			return fmt.Errorf("unknown command %q", args[0])
		}
	}()
	if err != nil {
		fmt.Fprintln(errw, "runtime:", err)
		return 1
	}
	return 0
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
