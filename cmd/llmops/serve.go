package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/otel"

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

		// Telemetry is wired here rather than inside runtime.Serve: the
		// process owns the exporter lifetime, and runtime.Serve is called
		// directly by tests that must not install a global provider.
		//
		// Bootstrap is a noop exporter-side unless OTEL_EXPORTER_OTLP_ENDPOINT
		// is set, so a bare-metal host with no collector pays nothing but the
		// local handler.
		logger, shutdown, err := otel.Bootstrap(ctx, otel.Config{
			ServiceName: "llmops",
			Version:     otel.Version(version),
			// The local handler writes to the command's error stream, which
			// is os.Stderr in production and a buffer under test. Letting it
			// default to os.Stderr would take the capture seam away.
			Stdout: slog.NewJSONHandler(errw, &slog.HandlerOptions{Level: slog.LevelInfo}),
		})
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				logger.Error("telemetry shutdown", "err", err)
			}
		}()
		if err != nil {
			// The OTLP log bridge failed; the logger still works, so serving
			// continues rather than refusing to start over telemetry.
			logger.Warn("telemetry log export unavailable", "err", err)
		}
		opts.Logger = logger
		logger.InfoContext(ctx, "serving",
			"manifest", *path, "model", m.Name, "port", *port, "engine_port", *enginePort)
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
		_, _ = fmt.Fprintf(out, "%s: ok\n", m.Name)
		return nil
	}

	ms, err := manifest.LoadDir(path)
	if err != nil {
		return err
	}
	for _, m := range ms {
		_, _ = fmt.Fprintf(out, "%s: ok (%s, %s, %s, %dx%s x%d)\n",
			m.Name, m.Runtime, m.DeployMode(), m.Format, m.GPU.Count, m.GPU.Type, m.GPU.Nodes)
	}
	_, _ = fmt.Fprintf(out, "%d manifests valid\n", len(ms))

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
	_, _ = fmt.Fprintf(out, "%d deploy artifacts consistent\n", len(ms))
	return nil
}
