package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/openaichat"
	"latere.ai/x/pkg/llmdialect/openairesp"

	"github.com/latere-ai/llmops/internal/manifest"
)

// Shim fronts the engine with the latere service contract
// (specs/003-serving-runtime.md):
//
//	GET /healthz  — 200 once the process is up
//	GET /ready    — 200 when weights are loaded AND the engine is healthy
//	GET /metrics  — engine Prometheus output + llmops_* gauges
//	anything else — reverse-proxied to the engine (token streaming safe)
type Shim struct {
	engine      *url.URL
	proxy       *httputil.ReverseProxy
	client      *http.Client // short-timeout, health/metrics only
	inference   *http.Client // no timeout: long generations
	surfaces    map[string]*surface
	weightsSecs atomic.Value // float64; 0 while loading
	lossCount   sync.Map     // lossKey -> *atomic.Int64

	// SystemPrompt, when set, is enforced on every chat request —
	// both dialect surfaces (specs/003).
	SystemPrompt *manifest.SystemPrompt

	// enginePath is where the engine listens for the dialect it speaks.
	enginePath string

	// HealthPath is the engine's health endpoint (default /health,
	// which SGLang and vLLM both expose; overridable for substitute
	// engines — specs/011).
	HealthPath string

	// Speculator names the draft-model configuration the engine was
	// started with, reported on every response (specs/027).
	//
	// It is a header rather than a suffix on the served model name:
	// which speculator is active changes throughput and can change the
	// tokens produced, so a measurement that does not record it cannot
	// be attributed — but the model being served is the same model, and
	// renaming it would break every caller's configuration whenever an
	// operator restarted with a different draft head.
	Speculator string
}

// SpeculatorHeader names the active draft-model configuration, or
// manifest.SpeculatorNone. It follows LossHeader (specs/025): an
// engine-side fact the caller cannot otherwise see, reported without
// changing the payload.
const SpeculatorHeader = "X-LLMOps-Speculator"

// NewShim fronts the engine at engineURL (e.g. http://127.0.0.1:30000).
func NewShim(engineURL string) (*Shim, error) {
	u, err := url.Parse(engineURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1 // flush every write: token streaming
	s := &Shim{
		engine:    u,
		proxy:     proxy,
		client:    &http.Client{Timeout: 5 * time.Second},
		inference: &http.Client{},
	}
	s.weightsSecs.Store(float64(0))
	if err := s.setDialect(manifest.EngineDialectOpenAIChat); err != nil {
		return nil, err
	}
	return s, nil
}

// surface is one caller-facing dialect: the path it answers on, the
// codec that decodes it, and the translator that drives the engine.
// Translator is nil when the caller speaks what the engine speaks —
// there is nothing to translate, so the request is proxied.
type surface struct {
	dialect    ir.Dialect
	translator *llmdialect.Translator
	native     bool
}

// frontends is the caller-side dialect table (specs/025). Adding a
// surface is a row here, not a branch in ServeHTTP.
//
// The Lux dialect is deliberately absent: that surface belongs to the
// gateway, which embeds the same package (specs/009). Serving it here
// would put one dialect in two places with two owners.
var frontends = []struct {
	path    string
	dialect ir.Dialect
	make    func() llmdialect.Frontend
}{
	{"/v1/chat/completions", ir.DialectOpenAIChat, func() llmdialect.Frontend { return openaichat.NewFrontend() }},
	{"/v1/messages", ir.DialectAnthropicMessages, func() llmdialect.Frontend { return anthropic.NewFrontend() }},
	{"/v1/responses", ir.DialectOpenAIResponses, func() llmdialect.Frontend { return openairesp.NewFrontend() }},
}

// backends maps the engine's declared dialect to the codec that encodes
// for it and the upstream path it listens on.
var backends = map[string]struct {
	dialect ir.Dialect
	path    string
	make    func() llmdialect.Backend
}{
	manifest.EngineDialectOpenAIChat: {ir.DialectOpenAIChat, "/v1/chat/completions",
		func() llmdialect.Backend { return openaichat.NewBackend(openaichat.BackendOptions{}) }},
	manifest.EngineDialectAnthropic: {ir.DialectAnthropicMessages, "/v1/messages",
		func() llmdialect.Backend { return anthropic.NewBackend(anthropic.BackendOptions{}) }},
	manifest.EngineDialectOpenAIResponses: {ir.DialectOpenAIResponses, "/v1/responses",
		func() llmdialect.Backend { return openairesp.NewBackend() }},
}

// setDialect binds every caller surface to the engine's dialect.
func (s *Shim) setDialect(engineDialect string) error {
	be, ok := backends[engineDialect]
	if !ok {
		return fmt.Errorf("engine dialect %q has no backend codec", engineDialect)
	}
	s.enginePath = be.path
	s.surfaces = make(map[string]*surface, len(frontends))
	for _, f := range frontends {
		sf := &surface{dialect: f.dialect, native: f.dialect == be.dialect}
		if !sf.native {
			sf.translator = &llmdialect.Translator{Frontend: f.make(), Backend: be.make()}
		}
		s.surfaces[f.path] = sf
	}
	return nil
}

// SetWeightsLoaded records the weight-preparation duration; readiness
// requires it to have been called.
func (s *Shim) SetWeightsLoaded(d time.Duration) {
	s.weightsSecs.Store(d.Seconds())
}

func (s *Shim) weightsLoaded() bool {
	return s.weightsSecs.Load().(float64) > 0
}

// EngineHealthy polls the engine's own health endpoint (both SGLang and
// vLLM expose GET /health).
func (s *Shim) EngineHealthy() bool {
	path := s.HealthPath
	if path == "" {
		path = "/health"
	}
	resp, err := s.client.Get(s.engine.String() + path)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func (s *Shim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set before dispatch so it reaches proxied responses too: the
	// reverse proxy copies the engine's headers in rather than
	// replacing what is already there. /v1/models therefore carries it
	// as well, which is what `llmops ps` reads.
	if s.Speculator != "" {
		w.Header().Set(SpeculatorHeader, s.Speculator)
	}
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	case "/ready":
		if s.weightsLoaded() && s.EngineHealthy() {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ready")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, "loading")
	case "/metrics":
		s.metrics(w)
	default:
		sf, ok := s.surfaces[r.URL.Path]
		switch {
		case !ok:
			s.proxy.ServeHTTP(w, r)
		case r.Method == http.MethodPost:
			s.dialectSurface(w, r, sf)
		case sf.native:
			// The engine owns this path; what it does with other methods
			// is its business, not ours to pre-empt.
			s.proxy.ServeHTTP(w, r)
		default:
			// A translated surface exists only here — the engine does not
			// serve this path at all, so proxying would 404 and blame the
			// wrong component.
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// dialectSurface serves one caller dialect. A surface the engine already
// speaks is proxied — translating a request into the dialect it is
// already in would cost a round trip through the IR and lose whatever
// the IR cannot carry, for nothing.
func (s *Shim) dialectSurface(w http.ResponseWriter, r *http.Request, sf *surface) {
	if sf.native && s.SystemPrompt == nil {
		s.proxy.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sf.native {
		injected, err := injectSystemPrompt(body, s.SystemPrompt)
		if err != nil {
			http.Error(w, fmt.Sprintf("inject system prompt: %v", err), http.StatusBadRequest)
			return
		}
		s.forward(w, r, s.enginePath, injected)
		return
	}
	s.translated(w, r, sf, body)
}

// forward posts body to the engine and streams the response back,
// flushing per chunk so SSE token streams are not buffered.
func (s *Shim) forward(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	up, err := s.engineRequest(r, path, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := s.inference.Do(up)
	if err != nil {
		http.Error(w, fmt.Sprintf("engine: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var dst io.Writer = w
	if fl, ok := w.(http.Flusher); ok {
		dst = flushWriter{w, fl}
	}
	_, _ = io.Copy(dst, resp.Body)
}

func (s *Shim) engineRequest(r *http.Request, path string, body []byte) (*http.Request, error) {
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.engine.String()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header = r.Header.Clone()
	up.Header.Set("Content-Type", "application/json")
	up.URL.RawQuery = r.URL.RawQuery
	return up, nil
}

type flushWriter struct {
	w  io.Writer
	fl http.Flusher
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.fl.Flush()
	return n, err
}

// injectSystemPrompt applies the manifest's system-prompt policy to an
// OpenAI Chat request body (both dialect surfaces funnel through the
// OpenAI shape before reaching the engine).
func injectSystemPrompt(body []byte, sp *manifest.SystemPrompt) ([]byte, error) {
	if sp == nil {
		return body, nil
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	msgs, _ := req["messages"].([]any)
	ours := map[string]any{"role": "system", "content": sp.Text}

	hasSystem := false
	var withoutSystem []any
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
			hasSystem = true
			continue
		}
		withoutSystem = append(withoutSystem, m)
	}

	switch sp.Mode {
	case manifest.SystemPromptDefault:
		if !hasSystem {
			req["messages"] = append([]any{ours}, msgs...)
		}
	case manifest.SystemPromptPrepend:
		req["messages"] = append([]any{ours}, msgs...)
	case manifest.SystemPromptOverride:
		req["messages"] = append([]any{ours}, withoutSystem...)
	default:
		return nil, fmt.Errorf("unknown system_prompt mode %q", sp.Mode)
	}
	return json.Marshal(req)
}

// anthropicMessages serves the Anthropic Messages dialect over the
// engine's OpenAI Chat endpoint, streaming included.
// LossHeader names the request fields a surface could not carry. It
// mirrors Lux's X-Lux-Compat-Loss so a client parsing one parses both.
const LossHeader = "X-LLMOps-Compat-Loss"

// translated serves a caller dialect the engine does not speak, through
// the shared IR (specs/025).
func (s *Shim) translated(w http.ResponseWriter, r *http.Request, sf *surface, body []byte) {
	out, req, err := sf.translator.Request(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("translate request: %v", err), http.StatusBadRequest)
		return
	}
	// llmdialect collects what a target dialect cannot represent rather
	// than dropping it silently. Discarding the report here would
	// reproduce the exact silent drop the package exists to prevent, so
	// the caller is told and the operator gets a count.
	if fields := req.Loss.Fields(); len(fields) > 0 {
		names := make([]string, len(fields))
		for i, f := range fields {
			names[i] = string(f)
		}
		w.Header().Set(LossHeader, strings.Join(names, ","))
		s.recordLoss(sf.dialect, names)
	}
	if out, err = injectSystemPrompt(out, s.SystemPrompt); err != nil {
		http.Error(w, fmt.Sprintf("inject system prompt: %v", err), http.StatusBadRequest)
		return
	}
	up, err := s.engineRequest(r, s.enginePath, out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := s.inference.Do(up)
	if err != nil {
		http.Error(w, fmt.Sprintf("engine: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if err := sf.translator.Stream(w, resp.Body); err != nil {
			return // stream already started; nothing safe to send
		}
		return
	}
	upstream, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	translated, err := sf.translator.Response(upstream)
	if err != nil {
		http.Error(w, fmt.Sprintf("translate response: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(translated)
}

type lossKey struct {
	dialect ir.Dialect
	field   string
}

// recordLoss counts how often a surface could not carry a field. How
// often callers ask for something a dialect cannot express is a fact
// about whether that surface is the right one.
func (s *Shim) recordLoss(d ir.Dialect, fields []string) {
	for _, f := range fields {
		v, _ := s.lossCount.LoadOrStore(lossKey{d, f}, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
}
func (s *Shim) metrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if resp, err := s.client.Get(s.engine.String() + "/metrics"); err == nil {
		if resp.StatusCode == http.StatusOK {
			_, _ = io.Copy(w, resp.Body)
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
		}
		_ = resp.Body.Close()
	}
	_, _ = fmt.Fprintf(w, "# HELP llmops_weights_load_seconds Time spent preparing weights before engine start.\n")
	_, _ = fmt.Fprintf(w, "# TYPE llmops_weights_load_seconds gauge\n")
	_, _ = fmt.Fprintf(w, "llmops_weights_load_seconds %g\n", s.weightsSecs.Load().(float64))

	// A label-only gauge, so a throughput panel can be broken down by
	// the speculator that produced it (specs/010, specs/027).
	if s.Speculator != "" {
		_, _ = fmt.Fprintf(w, "# HELP llmops_speculator_info The draft-model configuration the engine is serving with.\n")
		_, _ = fmt.Fprintf(w, "# TYPE llmops_speculator_info gauge\n")
		_, _ = fmt.Fprintf(w, "llmops_speculator_info{speculator=%q} 1\n", s.Speculator)
	}

	// One line per (surface, field) a caller asked for and the dialect
	// could not carry (specs/025).
	var lines []string
	s.lossCount.Range(func(k, v any) bool {
		key := k.(lossKey)
		lines = append(lines, fmt.Sprintf("llmops_dialect_loss_total{dialect=%q,field=%q} %d",
			string(key.dialect), key.field, v.(*atomic.Int64).Load()))
		return true
	})
	if len(lines) > 0 {
		sort.Strings(lines) // stable output; Range order is undefined
		_, _ = fmt.Fprintf(w, "# HELP llmops_dialect_loss_total Request fields a caller's dialect could not carry.\n")
		_, _ = fmt.Fprintf(w, "# TYPE llmops_dialect_loss_total counter\n")
		for _, l := range lines {
			_, _ = fmt.Fprintln(w, l)
		}
	}
}
