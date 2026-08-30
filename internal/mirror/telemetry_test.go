package mirror

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestHFClientPropagatesTraceContext: a weight pull that starts a new root
// trace cannot be told apart from a slow disk in the trace of whatever asked
// for it. http.DefaultClient, which this used to carry, sends no traceparent.
func TestHFClientPropagatesTraceContext(t *testing.T) {
	rec := installTracing(t)

	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("traceparent"):
		default:
		}
		_, _ = fmt.Fprint(w, `{"sha":"0123456789abcdef0123456789abcdef01234567"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewHFClient()
	c.Base = srv.URL
	if _, err := c.Resolve(context.Background(), "acme/tiny", "main"); err != nil {
		t.Fatal(err)
	}

	if n := clientSpans(rec); n != 1 {
		t.Fatalf("client spans = %d, want one per Hub request", n)
	}
	if tp := <-got; tp == "" {
		t.Fatal("the Hub request carried no traceparent")
	}
}
