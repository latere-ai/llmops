// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/latere-ai/llmops/internal/runtime"
)

// TestDiscoverReportsTheRunningSpeculator: the manifest offers a set and
// names a default, so only the running process knows which draft head is
// actually in use. Reading it from the endpoint is what keeps `ps` from
// reporting a default nobody started (specs/027).
func TestDiscoverReportsTheRunningSpeculator(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(runtime.SpeculatorHeader, "dflash2")
		switch r.URL.Path {
		case "/ready":
			_, _ = fmt.Fprintln(w, "ready")
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"qwen"}]}`)
		default:
			_, _ = fmt.Fprintln(w, "llmops_weights_load_seconds 12")
		}
	}))
	defer srv.Close()

	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", portOf(t, srv))

	got, err := Discover(context.Background(), cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Speculator != "dflash2" {
		t.Fatalf("Speculator = %q, want the one the endpoint reports", got[0].Speculator)
	}
}

// TestDiscoverLeavesTheSpeculatorUnknownWhenNothingAnswers: a model that
// is down must not be described as serving without speculation.
func TestDiscoverLeavesTheSpeculatorUnknownWhenNothingAnswers(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", 1) // nothing listening

	got, err := Discover(context.Background(), cfg, units, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != "down" {
		t.Fatalf("state = %q", got[0].State)
	}
	if got[0].Speculator != "" {
		t.Fatalf("Speculator = %q; a model that is down knows nothing", got[0].Speculator)
	}
}

// TestDiscoverDropsTheSpeculatorOfAnotherModel: when the port turns out
// to serve something else the whole reading is discarded, header
// included, rather than attributing a stranger's configuration.
func TestDiscoverDropsTheSpeculatorOfAnotherModel(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(runtime.SpeculatorHeader, "dspark")
		switch r.URL.Path {
		case "/ready":
			_, _ = fmt.Fprintln(w, "ready")
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"some-other-model"}]}`)
		default:
			_, _ = fmt.Fprintln(w, "llmops_weights_load_seconds 1")
		}
	}))
	defer srv.Close()

	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", portOf(t, srv))

	got, err := Discover(context.Background(), cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != "down" || got[0].Speculator != "" {
		t.Fatalf("got state=%q speculator=%q; another model's configuration leaked through",
			got[0].State, got[0].Speculator)
	}
}
