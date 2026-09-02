// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// installTracing points the global provider at an in-memory recorder and
// installs the W3C propagator, then restores both.
//
// otel.Transport resolves the globals, so a test that leaves them unset
// records nothing and every span assertion passes vacuously. That is exactly
// what a type assertion on the Transport field cannot catch, which is why the
// assertions below are on recorded spans and a received header.
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

// clientSpans counts the recorded spans of client kind.
func clientSpans(rec *tracetest.SpanRecorder) int {
	n := 0
	for _, sp := range rec.Ended() {
		if sp.SpanKind() == trace.SpanKindClient {
			n++
		}
	}
	return n
}

// TestDiscoverPropagatesTraceContext: `llmops ps` probes endpoints this fleet
// serves, so the probe and what it finds belong in one trace.
func TestDiscoverPropagatesTraceContext(t *testing.T) {
	rec := installTracing(t)

	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("traceparent")
		mu.Unlock()
		switch r.URL.Path {
		case "/ready":
			_, _ = fmt.Fprintln(w, "ready")
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"qwen"}]}`)
		case "/metrics":
			_, _ = fmt.Fprintln(w, "llmops_weights_load_seconds 12.5")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg, units := t.TempDir(), t.TempDir()
	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", portOf(t, srv))
	got, err := Discover(context.Background(), cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Ready() {
		t.Fatalf("discover = %+v", got)
	}

	// One probe is three requests: /ready, /v1/models, /metrics.
	if n := clientSpans(rec); n != 3 {
		t.Fatalf("client spans = %d, want one per probe request", n)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{"/ready", "/v1/models", "/metrics"} {
		if seen[p] == "" {
			t.Fatalf("probe of %s carried no traceparent", p)
		}
	}
}
