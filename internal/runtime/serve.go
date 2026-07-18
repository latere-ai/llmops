package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/latere-ai/open-llms/internal/manifest"
	"github.com/latere-ai/open-llms/internal/mirror"
)

// Options configure Serve. Zero values get production defaults.
type Options struct {
	Port       int    // shim listen port (default 8000)
	EnginePort int    // engine listen port (default 30000)
	CacheRoot  string // NVMe cache root (default /cache)
	// EngineCmd overrides the engine command for tests; "{model}" and
	// "{port}" are substituted. Production leaves it empty.
	EngineCmd []string
	Log       io.Writer
}

func (o *Options) defaults() {
	if o.Port == 0 {
		o.Port = 8000
	}
	if o.EnginePort == 0 {
		o.EnginePort = 30000
	}
	if o.CacheRoot == "" {
		o.CacheRoot = "/cache"
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
}

// EngineCommand renders the launch command for a manifest
// (specs/001 pins the engine versions inside the runtime images).
func EngineCommand(m *manifest.Manifest, model string, port int) ([]string, error) {
	p := strconv.Itoa(port)
	var cmd []string
	switch m.Runtime {
	case manifest.RuntimeSGLang:
		cmd = []string{"python3", "-m", "sglang.launch_server",
			"--model-path", model, "--host", "0.0.0.0", "--port", p}
	case manifest.RuntimeVLLM:
		cmd = []string{"vllm", "serve", model, "--host", "0.0.0.0", "--port", p}
		if m.Load == manifest.LoadS3Stream {
			cmd = append(cmd, "--load-format", "runai_streamer")
		}
	default:
		return nil, fmt.Errorf("runtime %q has no engine command (custom images launch themselves)", m.Runtime)
	}
	return append(cmd, m.Args...), nil
}

func renderOverride(tmpl []string, model string, port int) []string {
	out := make([]string, len(tmpl))
	for i, t := range tmpl {
		t = strings.ReplaceAll(t, "{model}", model)
		t = strings.ReplaceAll(t, "{port}", strconv.Itoa(port))
		out[i] = t
	}
	return out
}

// Serve runs the full entrypoint: prepare weights, start the engine,
// serve the shim. It returns when ctx is cancelled or the engine exits.
func Serve(ctx context.Context, m *manifest.Manifest, opts Options) error {
	opts.defaults()
	shim, err := NewShim(fmt.Sprintf("http://127.0.0.1:%d", opts.EnginePort))
	if err != nil {
		return err
	}
	shim.SystemPrompt = m.SystemPrompt

	// Expose /healthz (and a not-ready /ready) while weights load.
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", opts.Port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: shim}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	defer srv.Close()

	start := time.Now()
	store := mirror.OpenStore(m.S3Prefix)
	model, err := PrepareWeights(m, opts.CacheRoot, store, opts.Log)
	if err != nil {
		return fmt.Errorf("prepare weights: %w", err)
	}
	shim.SetWeightsLoaded(time.Since(start))
	fmt.Fprintf(opts.Log, "weights ready in %.1fs, launching %s\n", time.Since(start).Seconds(), m.Runtime)

	args := opts.EngineCmd
	if len(args) == 0 {
		if args, err = EngineCommand(m, model, opts.EnginePort); err != nil {
			return err
		}
	} else {
		args = renderOverride(args, model, opts.EnginePort)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = opts.Log, opts.Log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start engine: %w", err)
	}

	engineErr := make(chan error, 1)
	go func() { engineErr <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		<-engineErr // CommandContext kills the engine; reap it
		return nil
	case err := <-engineErr:
		return fmt.Errorf("engine exited: %w", err)
	case err := <-serveErr:
		return fmt.Errorf("shim server: %w", err)
	}
}
