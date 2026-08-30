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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
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

// serveForTracing starts Serve against a fake engine and returns the shim's
// base URL. The tracer provider must already be installed: otelhttp resolves
// it when the handler is built, not when a request arrives.
func serveForTracing(t *testing.T) string {
	t.Helper()
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
	base := serveForTracing(t)

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
