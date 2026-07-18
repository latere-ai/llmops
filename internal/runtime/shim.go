package runtime

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
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
	client      *http.Client
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
		engine: u,
		proxy:  proxy,
		client: &http.Client{Timeout: 5 * time.Second},
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
	default:
		s.proxy.ServeHTTP(w, r)
	}
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
