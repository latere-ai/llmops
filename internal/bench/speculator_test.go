package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// speculativeEngine emits several tokens per SSE event, the way an
// engine committing a verified draft block does, and closes with the
// usage record.
func speculativeEngine(t *testing.T, events, tokensPerEvent int, speculator string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if speculator != "" {
			w.Header().Set(SpeculatorHeader, speculator)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for range events {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"a b c d"}}]}`+"\n\n")
			fl.Flush()
		}
		// Only sent because the client asked for it.
		if opts, ok := req["stream_options"].(map[string]any); ok && opts["include_usage"] == true {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":%d}}\n\n",
				events*tokensPerEvent)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTokensAreCountedNotEvents is the measurement bug this guards
// against. Speculative decoding commits several tokens per verify step,
// so counting server-sent events understates throughput — a published
// figure for this model was wrong for exactly that reason.
func TestTokensAreCountedNotEvents(t *testing.T) {
	srv := speculativeEngine(t, 5, 4, "dspark")
	rep, err := Run(context.Background(), Config{
		BaseURL: srv.URL, Model: "tiny", Requests: 3, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Chunks != 3*6 { // 5 content events + 1 usage event per request
		t.Fatalf("chunks = %d", rep.Chunks)
	}
	if rep.Tokens != 3*20 {
		t.Fatalf("tokens = %d, want the engine's own count", rep.Tokens)
	}
	if rep.TokensPerS <= rep.ChunksPerS {
		t.Fatalf("tokens/s %.1f must exceed chunks/s %.1f when an event carries several tokens",
			rep.TokensPerS, rep.ChunksPerS)
	}
}

// TestReportRecordsTheSpeculator: the same endpoint answers at very
// different rates depending on the draft head, so a report that does
// not name it cannot be compared to another (specs/027 AC3).
func TestReportRecordsTheSpeculator(t *testing.T) {
	srv := speculativeEngine(t, 2, 3, "dflash2")
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "tiny", Requests: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Speculator != "dflash2" {
		t.Fatalf("Speculator = %q", rep.Speculator)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"speculator":"dflash2"`, `"tokens_per_s"`, `"tokens"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("report JSON missing %s: %s", key, data)
		}
	}
}

// TestReportOmitsAnAbsentSpeculator keeps the field out of reports from
// endpoints that are not ours, rather than recording an empty claim.
func TestReportOmitsAnAbsentSpeculator(t *testing.T) {
	srv := speculativeEngine(t, 2, 3, "")
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "tiny", Requests: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Speculator != "" {
		t.Fatalf("Speculator = %q", rep.Speculator)
	}
	if data, _ := json.Marshal(rep); strings.Contains(string(data), "speculator") {
		t.Fatalf("absent speculator was serialized: %s", data)
	}
}

// TestSpeculatorChangeMidRunFails: if the endpoint restarted with a
// different head partway through, the aggregate describes two
// configurations at once and is worth nothing.
func TestSpeculatorChangeMidRunFails(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		spec := "dspark"
		if n > 1 {
			spec = "dflash2"
		}
		w.Header().Set(SpeculatorHeader, spec)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	_, err := Run(context.Background(), Config{
		BaseURL: srv.URL, Model: "tiny", Requests: 4, Concurrency: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "changed speculator mid-run") {
		t.Fatalf("err = %v", err)
	}
}

// TestUsageAbsentLeavesTokensZero: an endpoint that ignores
// stream_options still yields a usable latency report rather than a
// fabricated token count.
func TestUsageAbsentLeavesTokensZero(t *testing.T) {
	srv, _ := fakeOpenAI(t, 3, http.StatusOK)
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "tiny", Requests: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tokens != 0 || rep.TokensPerS != 0 {
		t.Fatalf("tokens = %d, tokens/s = %v; want zero rather than a guess", rep.Tokens, rep.TokensPerS)
	}
	if rep.ChunksPerS == 0 {
		t.Fatal("latency reporting stopped working without usage")
	}
}
