package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/openaichat"

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
	translator  *llmdialect.Translator
	weightsSecs atomic.Value // float64; 0 while loading

	// SystemPrompt, when set, is enforced on every chat request —
	// both dialect surfaces (specs/003).
	SystemPrompt *manifest.SystemPrompt

	// HealthPath is the engine's health endpoint (default /health,
	// which SGLang and vLLM both expose; overridable for substitute
	// engines — specs/011).
	HealthPath string
}

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
		// Anthropic Messages callers drive the engine's OpenAI Chat
		// surface through the shared dialect translator (latere.ai/x/pkg/llmdialect).
		// The Lux dialect is deliberately absent: that surface belongs to
		// the gateway, which embeds the same package (specs/009).
		translator: &llmdialect.Translator{
			Frontend: anthropic.NewFrontend(),
			Backend:  openaichat.NewBackend(openaichat.BackendOptions{}),
		},
	}
	s.weightsSecs.Store(float64(0))
	return s, nil
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
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func (s *Shim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	case "/ready":
		if s.weightsLoaded() && s.EngineHealthy() {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ready")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "loading")
	case "/metrics":
		s.metrics(w)
	case "/anthropic/v1/messages":
		s.anthropicMessages(w, r)
	case "/v1/chat/completions":
		if s.SystemPrompt != nil && r.Method == http.MethodPost {
			s.chatCompletions(w, r)
			return
		}
		s.proxy.ServeHTTP(w, r)
	default:
		s.proxy.ServeHTTP(w, r)
	}
}

// chatCompletions intercepts the OpenAI surface only when a system
// prompt must be enforced; otherwise the transparent proxy handles it.
func (s *Shim) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	injected, err := injectSystemPrompt(body, s.SystemPrompt)
	if err != nil {
		http.Error(w, fmt.Sprintf("inject system prompt: %v", err), http.StatusBadRequest)
		return
	}
	s.forward(w, r, "/v1/chat/completions", injected)
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
	defer resp.Body.Close()
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
	io.Copy(dst, resp.Body)
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
func (s *Shim) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, req, err := s.translator.Request(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("translate request: %v", err), http.StatusBadRequest)
		return
	}
	if out, err = injectSystemPrompt(out, s.SystemPrompt); err != nil {
		http.Error(w, fmt.Sprintf("inject system prompt: %v", err), http.StatusBadRequest)
		return
	}
	up, err := s.engineRequest(r, "/v1/chat/completions", out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := s.inference.Do(up)
	if err != nil {
		http.Error(w, fmt.Sprintf("engine: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if err := s.translator.Stream(w, resp.Body); err != nil {
			return // stream already started; nothing safe to send
		}
		return
	}
	upstream, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	translated, err := s.translator.Response(upstream)
	if err != nil {
		http.Error(w, fmt.Sprintf("translate response: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(translated)
}

// metrics passes the engine's Prometheus output through and appends the
// runtime's own gauges. The endpoint stays serviceable while the engine
// is still starting.
func (s *Shim) metrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if resp, err := s.client.Get(s.engine.String() + "/metrics"); err == nil {
		if resp.StatusCode == http.StatusOK {
			io.Copy(w, resp.Body)
		} else {
			io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
	}
	fmt.Fprintf(w, "# HELP llmops_weights_load_seconds Time spent preparing weights before engine start.\n")
	fmt.Fprintf(w, "# TYPE llmops_weights_load_seconds gauge\n")
	fmt.Fprintf(w, "llmops_weights_load_seconds %g\n", s.weightsSecs.Load().(float64))
}
