package runtime

import (
	"bytes"
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
)

// Shim fronts the engine with the latere service contract
// (specs/003-serving-runtime.md, mirroring ../ocrmodel):
//
//	GET /healthz  — 200 once the process is up
//	GET /ready    — 200 when weights are loaded AND the engine is healthy
//	GET /metrics  — engine Prometheus output + openllms_* gauges
//	anything else — reverse-proxied to the engine (token streaming safe)
type Shim struct {
	engine      *url.URL
	proxy       *httputil.ReverseProxy
	client      *http.Client // short-timeout, health/metrics only
	inference   *http.Client // no timeout: long generations
	translator  *llmdialect.Translator
	weightsSecs atomic.Value // float64; 0 while loading
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
		// surface through the shared dialect translator (../pkg/llmdialect).
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
	resp, err := s.client.Get(s.engine.String() + "/health")
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
	default:
		s.proxy.ServeHTTP(w, r)
	}
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
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.engine.String()+"/v1/chat/completions", bytes.NewReader(out))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	up.Header.Set("Content-Type", "application/json")
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
		io.Copy(w, resp.Body)
		resp.Body.Close()
	}
	fmt.Fprintf(w, "# HELP openllms_weights_load_seconds Time spent preparing weights before engine start.\n")
	fmt.Fprintf(w, "# TYPE openllms_weights_load_seconds gauge\n")
	fmt.Fprintf(w, "openllms_weights_load_seconds %g\n", s.weightsSecs.Load().(float64))
}
