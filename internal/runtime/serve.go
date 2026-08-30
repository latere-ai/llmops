package runtime

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/otel"

	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/mirror"
)

// Options configure Serve. Zero values get production defaults.
type Options struct {
	Port       int // shim listen port (default 8000)
	EnginePort int // engine listen port (default 30000)
	// CacheRoot is where weights live on this host: the fetch target
	// for nvme-cache, and the directory verified in place for
	// load: local (specs/021). Default /cache suits the container
	// images; a bare-metal host points it somewhere like ~/.models.
	CacheRoot string
	// EngineCmd overrides the engine command for tests; "{model}" and
	// "{port}" are substituted. Production leaves it empty.
	EngineCmd []string
	// Speculator names which of the manifest's speculators to serve
	// with. Empty takes the manifest's default; manifest.SpeculatorNone
	// serves without speculation (specs/027).
	Speculator string
	// Log is the byte stream the engine's stdout and stderr are wired to,
	// and the destination of the weight-preparation progress lines. It
	// stays an io.Writer because exec.Cmd needs one.
	Log io.Writer
	// Logger records the events of the serve lifecycle. Defaults to the
	// slog default, which otel.Bootstrap has replaced in the CLI.
	Logger *slog.Logger
}

func (o *Options) defaults() {
	o.Port = cmp.Or(o.Port, 8000)
	o.EnginePort = cmp.Or(o.EnginePort, 30000)
	o.CacheRoot = expandHome(cmp.Or(o.CacheRoot, "/cache"))
	if o.Log == nil {
		o.Log = os.Stderr
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// expandHome resolves a leading ~/ in a path. systemd does not expand
// tilde in ExecStart, so a unit written with --cache-root ~/.models
// would otherwise create a directory literally named "~" beside the
// working directory and silently look for weights in it.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
}

// EngineCommand renders the launch command for a manifest
// (specs/001 pins the engine versions inside the runtime images).
//
// spec is the resolved speculator choice, and draftPath the directory
// its head was staged into — empty when speculation is off or the head
// lives in the target checkpoint.
func EngineCommand(m *manifest.Manifest, model string, port int, spec manifest.Speculation, draftPath string) ([]string, error) {
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
	// Serve under the manifest name, not the weights path — callers
	// address models as llmops/<name> and both engines 404 unknown
	// model ids otherwise.
	cmd = append(cmd, "--served-model-name", m.Name)
	cmd = append(cmd, m.Args...)

	// The speculator's flags come last so several speculators can share
	// one base configuration, and the draft path is supplied here rather
	// than written in the manifest because it depends on this host's
	// cache root (specs/021).
	cmd = append(cmd, spec.Speculator.Args...)
	if draftPath != "" {
		cmd = append(cmd, "--speculative-draft-model-path="+draftPath)
	}
	return cmd, nil
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

// probeRequest reports whether a request is machinery rather than a
// caller. The kubelet polls /healthz and /ready on every pod on a
// seconds-long period (deploy/*/lws.yaml), and `llmops ps` scrapes
// /metrics on every invocation. A span each would outnumber the
// inference spans the trace exists to show.
func probeRequest(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/ready", "/metrics":
		return true
	}
	return false
}

// Serve runs the full entrypoint: prepare weights, start the engine,
// serve the shim. It returns when ctx is cancelled or the engine exits.
func Serve(ctx context.Context, m *manifest.Manifest, opts Options) error {
	opts.defaults()
	// Resolve the speculator before anything is started: an unknown name
	// should cost nothing, and the choice decides which weights to stage.
	spec, err := m.ResolveSpeculator(opts.Speculator)
	if err != nil {
		return err
	}
	shim, err := NewShim(fmt.Sprintf("http://127.0.0.1:%d", opts.EnginePort))
	if err != nil {
		return err
	}
	if err := shim.setDialect(m.Dialect()); err != nil {
		return err
	}
	shim.Speculator = spec.Name
	shim.SystemPrompt = m.SystemPrompt
	shim.HealthPath = os.Getenv("LLMOPS_ENGINE_HEALTH_PATH")

	// After the shim's fields are set and before it serves: the metric
	// callback reads them when a collection runs.
	unregister, err := shim.registerMetrics()
	if err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}
	defer func() { _ = unregister() }()

	// Expose /healthz (and a not-ready /ready) while weights load.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf(":%d", opts.Port))
	if err != nil {
		return err
	}
	// The shim is wrapped once, here, rather than inside NewShim: a
	// handler wrapped twice reports two server spans per request, and
	// this is the only place a listener is attached to it.
	srv := &http.Server{Handler: otel.Handler(shim, "llmops",
		otel.WithSkip(probeRequest),
		otel.WithRouteTemplate(shim.RouteTemplate),
	)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	start := time.Now()
	store := mirror.OpenStore(m.S3Prefix)
	model, err := PrepareWeights(m, opts.CacheRoot, store, opts.Log)
	if err != nil {
		return fmt.Errorf("prepare weights: %w", err)
	}
	draft, err := PrepareDraft(spec.Speculator, m.Load, opts.CacheRoot, mirror.OpenStore(spec.Speculator.S3Prefix), opts.Log)
	if err != nil {
		return fmt.Errorf("prepare draft head for speculator %q: %w", spec.Name, err)
	}

	// After the weights are on disk and before the engine allocates:
	// this is the moment the checkpoint is in page cache and the engine
	// is about to claim its fraction of the same pool.
	if err := CheckMemoryBudget(m, model, opts.Log); err != nil {
		return err
	}
	shim.SetWeightsLoaded(time.Since(start))
	opts.Logger.InfoContext(ctx, "weights ready, launching engine",
		"model", m.Name,
		"seconds", time.Since(start).Seconds(),
		"runtime", m.Runtime,
		"speculator", spec.Name)

	args := opts.EngineCmd
	if len(args) == 0 {
		if args, err = EngineCommand(m, model, opts.EnginePort, spec, draft); err != nil {
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
