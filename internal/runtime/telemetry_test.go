package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
