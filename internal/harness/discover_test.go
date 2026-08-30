package harness

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func writeInstalled(t *testing.T, configDir, name string) {
	t.Helper()
	data := `name: ` + name + `
hf_repo: acme/tiny
revision: ` + sha + `
format: bf16
license: mit
runtime: vllm
deploy: bare-metal
load: local
gpu: {type: gb10, count: 1, nodes: 1}
context_max: 4096
args: ["--max-model-len=4096", "--gpu-memory-utilization=0.65"]
`
	if err := os.WriteFile(filepath.Join(configDir, name+".yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeUnitWithPort(t *testing.T, unitDir, name string, port int) {
	t.Helper()
	body := fmt.Sprintf("[Service]\nExecStart=/usr/local/bin/llmops serve "+
		"--manifest /etc/llmops/%s.yaml --port %d\n", name, port)
	if err := os.WriteFile(filepath.Join(unitDir, name+".service"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeShim answers the endpoints Discover probes.
func fakeShim(t *testing.T, state string, loaded float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			if state != "ready" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			_, _ = fmt.Fprintln(w, state)
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"qwen"}]}`)
		case "/metrics":
			_, _ = fmt.Fprintf(w, "llmops_weights_load_seconds %g\n", loaded)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	var p int
	if _, err := fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoverReportsWhatIsAnswering(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	srv := fakeShim(t, "ready", 39.2)
	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", portOf(t, srv))

	got, err := Discover(cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("discovered %d models", len(got))
	}
	m := got[0]
	if !m.Ready() {
		t.Fatalf("state %q, want ready", m.State)
	}
	if m.Loaded != 39.2 {
		t.Errorf("loaded %v, want 39.2 from the metrics gauge", m.Loaded)
	}
	if m.GPU != "1xgb10" || m.Runtime != "vllm" {
		t.Errorf("model shape wrong: %+v", m)
	}
}

// TestDiscoverShowsInstalledButDownModels is specs/026 AC1: a model
// whose unit exists but is not answering must be listed as down, not
// omitted. "Configured but not running" is the state an operator most
// needs to see, and hiding it looks like the model was never installed.
func TestDiscoverShowsInstalledButDownModels(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", 1) // nothing listens on port 1

	got, err := Discover(cfg, units, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a down model was omitted: %+v", got)
	}
	if got[0].State != "down" {
		t.Fatalf("state %q, want down", got[0].State)
	}
}

func TestDiscoverReportsLoading(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	srv := fakeShim(t, "loading", 0)
	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", portOf(t, srv))

	got, err := Discover(cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != "loading" || got[0].Ready() {
		t.Fatalf("state %q, want loading and not ready", got[0].State)
	}
}

// TestPortComesFromTheUnit pins where the port lives. It is a property
// of the deployment, so the unit carries it; a manifest field would put
// the same fact in two places.
func TestPortComesFromTheUnit(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", 9123)
	got, err := Discover(cfg, units, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Port != 9123 {
		t.Fatalf("port %d, want 9123 from the unit", got[0].Port)
	}

	// The equals form is equally valid in a unit file.
	body := "[Service]\nExecStart=/usr/local/bin/llmops serve --manifest /etc/llmops/qwen.yaml --port=9124\n"
	if err := os.WriteFile(filepath.Join(units, "qwen.service"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = Discover(cfg, units, 100*time.Millisecond)
	if got[0].Port != 9124 {
		t.Fatalf("port %d, want 9124 from the --port= form", got[0].Port)
	}
}

func TestPortFallsBackWhenNoUnit(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	writeInstalled(t, cfg, "qwen") // started by hand: no unit at all
	got, err := Discover(cfg, units, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Port != DefaultPort {
		t.Fatalf("port %d, want the serve default %d", got[0].Port, DefaultPort)
	}
}

func TestEndpointForUsesTheDiscoveredPort(t *testing.T) {
	e := EndpointFor(Model{Name: "qwen", Port: 9000}, "box", "local")
	if e.BaseURL != "http://box:9000" || e.Model != "qwen" {
		t.Fatalf("endpoint %+v", e)
	}
}

// TestDiscoverRejectsAPortServingAnotherModel is the bug running this
// against a real host exposed: without a unit a model falls back to the
// default port, so every manifest pointed at one address and whatever
// answered there was reported as all of them.
func TestDiscoverRejectsAPortServingAnotherModel(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	got, err := Discover(cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != "down" {
		t.Fatalf("state %q: a port serving another model was reported as this one", got[0].State)
	}
}

// TestDiscoverTrustsAnEngineWithoutAModelList keeps the identity check
// from becoming a requirement on the engine's API surface.
func TestDiscoverTrustsAnEngineWithoutAModelList(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			_, _ = fmt.Fprintln(w, "ready")
		case "/v1/models":
			http.NotFound(w, r)
		default:
			_, _ = fmt.Fprintln(w, "llmops_weights_load_seconds 5")
		}
	}))
	defer srv.Close()

	writeInstalled(t, cfg, "qwen")
	writeUnitWithPort(t, units, "qwen", portOf(t, srv))

	got, err := Discover(cfg, units, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Ready() {
		t.Fatalf("state %q: an engine without /v1/models should get the benefit of the doubt", got[0].State)
	}
}
