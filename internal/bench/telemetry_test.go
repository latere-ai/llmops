package bench

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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

// TestRunPropagatesTraceContext: the bench measures a served endpoint, so its
// requests must join the trace of the serving they provoke. Without it a slow
// sample is a number with no server or engine span underneath it.
func TestRunPropagatesTraceContext(t *testing.T) {
	rec := installTracing(t)

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("traceparent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	if _, err := Run(context.Background(), Config{
		BaseURL: srv.URL, Model: "tiny", Concurrency: 1, Requests: 2, Prompt: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	if n := clientSpans(rec); n != 2 {
		t.Fatalf("client spans = %d, want one per request", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests", len(seen))
	}
	for i, tp := range seen {
		if tp == "" {
			t.Fatalf("request %d carried no traceparent", i)
		}
	}
}
