package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"latere.ai/x/pkg/llmdialect/ir"
)

// TestServeLogsLifecycleStructured asserts the serve lifecycle reaches the
// injected logger as structured records rather than free text on the byte
// writer. The fields are the ones an operator filters on: which model, which
// speculator, how long the weights took.
func TestServeLogsLifecycleStructured(t *testing.T) {
	store := seedStore(t, weights)
	m := testManifest(store.Root)

	healthy := true
	engine := fakeEngine(t, &healthy)
	engineURL, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatalf("parse engine URL: %v", err)
	}
	enginePort, err := strconv.Atoi(engineURL.Port())
	if err != nil {
		t.Fatalf("engine port %q: %v", engineURL.Port(), err)
	}

	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	shimPort := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, m, Options{
			Port:       shimPort,
			EnginePort: enginePort,
			CacheRoot:  t.TempDir(),
			EngineCmd:  []string{"sleep", "60"},
			Log:        io.Discard,
			Logger:     logger,
		})
	}()

	waitFor(t, fmt.Sprintf("http://127.0.0.1:%d/ready", shimPort), 200, 5*time.Second)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}

	rec := findRecord(t, logs.String(), "weights ready, launching engine")
	for _, k := range []string{"model", "seconds", "runtime", "speculator"} {
		if _, ok := rec[k]; !ok {
			t.Fatalf("record missing %q: %v", k, rec)
		}
	}
	if rec["model"] != m.Name {
		t.Fatalf("model = %v, want %s", rec["model"], m.Name)
	}
	if s, ok := rec["seconds"].(float64); !ok || s < 0 {
		t.Fatalf("seconds = %v, want a non-negative number", rec["seconds"])
	}
}

// TestServeDefaultsToTheSlogDefault covers the seam a caller that never sets
// Logger relies on: otel.Bootstrap installs the default, so Serve must use it
// rather than dropping the events.
func TestServeDefaultsToTheSlogDefault(t *testing.T) {
	o := Options{}
	o.defaults()
	if o.Logger != slog.Default() {
		t.Fatal("Options.defaults must fall back to the slog default")
	}
	if o.Log == nil {
		t.Fatal("Options.defaults must keep the io.Writer seam")
	}
}

// findRecord returns the first JSON log record whose msg matches.
func findRecord(t *testing.T, out, msg string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q in:\n%s", msg, out)
	return nil
}

// syncBuffer is a bytes.Buffer safe for the writer goroutine inside Serve and
// the test goroutine reading it after the server stops.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// installTracing points the global provider at an in-memory recorder and
// installs the W3C propagator, then restores both.
//
// The globals are what otel.Handler and otel.Transport resolve, so a test that
// does not install them records nothing and every span assertion passes
// vacuously. That is the failure mode a type assertion on the transport cannot
// see, which is why the assertions below are on recorded spans.
func installTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(rec),
	)
	prevTP, prevProp := otelapi.GetTracerProvider(), otelapi.GetTextMapPropagator()
	otelapi.SetTracerProvider(tp)
	otelapi.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otelapi.SetTracerProvider(prevTP)
		otelapi.SetTextMapPropagator(prevProp)
	})
	return rec
}

// serveForTracing starts Serve against engine and returns the shim's base URL.
// The tracer provider must already be installed: otelhttp resolves it when the
// handler is built, not when a request arrives.
func serveForTracing(t *testing.T, engine *httptest.Server) string {
	t.Helper()
	store := seedStore(t, weights)
	m := testManifest(store.Root)

	engineURL, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatalf("parse engine URL: %v", err)
	}
	enginePort, err := strconv.Atoi(engineURL.Port())
	if err != nil {
		t.Fatalf("engine port %q: %v", engineURL.Port(), err)
	}

	shimPort := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, m, Options{
			Port:       shimPort,
			EnginePort: enginePort,
			CacheRoot:  t.TempDir(),
			EngineCmd:  []string{"sleep", "60"},
			Log:        io.Discard,
			Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", shimPort)
	waitFor(t, base+"/ready", 200, 5*time.Second)
	return base
}

// TestServeTracesRequestsAndSkipsProbes asserts both halves in one test with
// the provider installed: an inference request produces a server span, and the
// probe endpoints produce none. Asserting the skip alone would pass in a
// process with no provider, where nothing is recorded either way.
func TestServeTracesRequestsAndSkipsProbes(t *testing.T) {
	rec := installTracing(t)
	healthy := true
	base := serveForTracing(t, fakeEngine(t, &healthy))

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"tiny","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	for _, p := range []string{"/healthz", "/ready", "/metrics"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	var server []string
	for _, sp := range rec.Ended() {
		if sp.SpanKind() == trace.SpanKindServer {
			server = append(server, sp.Name()+" "+routeAttr(sp))
		}
	}
	if len(server) != 1 {
		t.Fatalf("server spans = %v, want exactly the inference request", server)
	}
	if !strings.Contains(server[0], "/v1/chat/completions") {
		t.Fatalf("server span = %q, want the chat surface", server[0])
	}
}

// routeAttr returns the http.route attribute of a recorded span.
func routeAttr(sp sdktrace.ReadOnlySpan) string {
	for _, a := range sp.Attributes() {
		if a.Key == "http.route" {
			return a.Value.AsString()
		}
	}
	return ""
}

// TestRouteTemplateBoundsCardinality: the shim proxies whatever the engine
// serves, so an unbounded path would become an unbounded span attribute.
func TestRouteTemplateBoundsCardinality(t *testing.T) {
	s, err := NewShim("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/healthz":              "/healthz",
		"/metrics":              "/metrics",
		"/v1/models":            "/v1/models",
		"/v1/chat/completions":  "/v1/chat/completions",
		"/v1/messages":          "/v1/messages",
		"/generate/abc-123-xyz": "/proxy",
	} {
		if got := s.RouteTemplate(httptest.NewRequest(http.MethodGet, path, nil)); got != want {
			t.Errorf("RouteTemplate(%q) = %q, want %q", path, got, want)
		}
	}
}

// traceEngine stands in for the inference engine and keeps the headers of
// every request the shim sent it.
type traceEngine struct {
	mu      sync.Mutex
	headers map[string]http.Header
}

func newTraceEngine(t *testing.T) (*httptest.Server, *traceEngine) {
	t.Helper()
	e := &traceEngine{headers: map[string]http.Header{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.headers[r.URL.Path] = r.Header.Clone()
		e.mu.Unlock()
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, e
}

func (e *traceEngine) traceparent(path string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.headers[path].Get("traceparent")
}

// TestShimPropagatesTraceContextUpstream is the end-to-end claim: a caller's
// request produces a server span, the engine hop produces a client span in the
// same trace, and the engine receives a traceparent naming it. Both routes to
// the engine are covered, because the reverse proxy carries the default
// deployment and the forwarding client carries the translated surfaces.
func TestShimPropagatesTraceContextUpstream(t *testing.T) {
	rec := installTracing(t)
	engine, recorded := newTraceEngine(t)
	base := serveForTracing(t, engine)

	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		resp, err := http.Post(base+path, "application/json",
			strings.NewReader(`{"model":"tiny","max_tokens":16,`+
				`"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// The engine speaks OpenAI chat, so both caller surfaces land on the
	// same upstream path: one proxied, one forwarded after translation.
	tp := recorded.traceparent("/v1/chat/completions")
	if tp == "" {
		t.Fatal("engine received no traceparent: the upstream hop starts a new trace")
	}

	// Group by trace: each caller request is its own trace, and the engine
	// hop must land inside the trace its caller opened.
	//
	// Client spans without a parent are the engine health poll, which runs
	// under the skipped /ready path and so has no caller to belong to.
	serverTraces := map[string]int{}
	for _, sp := range rec.Ended() {
		if sp.SpanKind() == trace.SpanKindServer {
			serverTraces[sp.SpanContext().TraceID().String()] = 0
		}
	}
	if len(serverTraces) != 2 {
		t.Fatalf("server spans = %d, want one per caller request", len(serverTraces))
	}
	for _, sp := range rec.Ended() {
		if sp.SpanKind() != trace.SpanKindClient || !sp.Parent().IsValid() {
			continue
		}
		id := sp.SpanContext().TraceID().String()
		if _, ok := serverTraces[id]; !ok {
			t.Fatalf("client span trace %s belongs to no server span: the hop is not linked", id)
		}
		serverTraces[id]++
	}
	for id, n := range serverTraces {
		if n == 0 {
			t.Fatalf("trace %s has a server span but no engine hop", id)
		}
	}
	if _, ok := serverTraces[traceIDOf(tp)]; !ok {
		t.Fatalf("traceparent %q carries no trace the shim opened (%v)", tp, serverTraces)
	}
}

// traceIDOf pulls the trace id out of a W3C traceparent header
// (version-traceid-spanid-flags).
func traceIDOf(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// TestShimExportsGaugesAsInstruments asserts the llmops_* facts reach an OTel
// meter as well as the Prometheus text endpoint. The text endpoint stays
// because `llmops ps` reads llmops_weights_load_seconds out of it, so both
// have to hold at once.
func TestShimExportsGaugesAsInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otelapi.GetMeterProvider()
	otelapi.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otelapi.SetMeterProvider(prev)
	})

	s, err := NewShim("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	s.Speculator = "eagle"
	unregister, err := s.registerMetrics()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unregister() }()

	s.SetWeightsLoaded(39200 * time.Millisecond)
	s.recordLoss(context.Background(), ir.DialectAnthropicMessages, []string{"top_k"})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = true
		}
	}
	for _, want := range []string{
		"llmops.weights.load.duration",
		"llmops.speculator.info",
		"llmops.dialect.loss",
	} {
		if !got[want] {
			t.Errorf("instrument %q not collected; got %v", want, got)
		}
	}

	// The text endpoint is the CLI's own API and must still answer.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "llmops_weights_load_seconds 39.2") {
		t.Fatalf("text endpoint lost the gauge llmops ps reads:\n%s", rec.Body.String())
	}
}
