package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOpenAI streams n SSE chunks per request.
func fakeOpenAI(t *testing.T, chunks int, status int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			http.Error(w, "expected stream", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestRunHappyPath(t *testing.T) {
	srv, calls := fakeOpenAI(t, 5, http.StatusOK)
	rep, err := Run(context.Background(), Config{
		BaseURL: srv.URL, Model: "tiny", Concurrency: 4, Requests: 12, Prompt: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 0 || rep.Requests != 12 || rep.Chunks != 60 {
		t.Fatalf("report %+v", rep)
	}
	if calls.Load() != 12 {
		t.Fatalf("server saw %d calls", calls.Load())
	}
	if rep.TTFTp50Ms <= 0 || rep.TTFTp95Ms < rep.TTFTp50Ms || rep.ChunksPerS <= 0 {
		t.Fatalf("stats %+v", rep)
	}
}

func TestRunReportIsStableJSON(t *testing.T) {
	srv, _ := fakeOpenAI(t, 2, http.StatusOK)
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "tiny", Requests: 2})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ttft_p50_ms", "ttft_p95_ms", "chunks_per_s", "duration_s", "config"} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("report JSON missing %q: %s", key, data)
		}
	}
}

func TestRunServerErrors(t *testing.T) {
	srv, _ := fakeOpenAI(t, 0, http.StatusInternalServerError)
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "tiny", Requests: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 3 {
		t.Fatalf("errors = %d, want 3", rep.Errors)
	}
}

func TestRunEmptyStreamIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "tiny", Requests: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 1 {
		t.Fatalf("empty stream not counted as error: %+v", rep)
	}
}

func TestRunConnectionRefused(t *testing.T) {
	rep, err := Run(context.Background(), Config{
		BaseURL: "http://127.0.0.1:1", Model: "tiny", Requests: 2, TimeoutSecs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 2 {
		t.Fatalf("connection errors not counted: %+v", rep)
	}
}

func TestRunValidation(t *testing.T) {
	if _, err := Run(context.Background(), Config{}); err == nil {
		t.Fatal("missing base_url/model must error")
	}
}

func TestRunDefaults(t *testing.T) {
	srv, calls := fakeOpenAI(t, 1, http.StatusOK)
	rep, err := Run(context.Background(), Config{BaseURL: srv.URL, Model: "m"})
	if err != nil || rep.Requests != 1 || calls.Load() != 1 {
		t.Fatalf("defaults: %+v, %v, calls=%d", rep, err, calls.Load())
	}
}

func TestPercentile(t *testing.T) {
	d := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(d, 50); p != 6 {
		t.Fatalf("p50 = %d", p)
	}
	if p := percentile(d, 95); p != 9 {
		t.Fatalf("p95 = %d", p)
	}
	if p := percentile([]time.Duration{7}, 95); p != 7 {
		t.Fatalf("single p95 = %d", p)
	}
}
