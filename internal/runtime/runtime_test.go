package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/open-llms/internal/manifest"
	"github.com/latere-ai/open-llms/internal/mirror"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// seedStore builds a completed mirror in a LocalStore.
func seedStore(t *testing.T, files map[string]string) *mirror.LocalStore {
	t.Helper()
	root := t.TempDir()
	store := &mirror.LocalStore{Root: root}
	var entries []mirror.FileEntry
	var total int64
	for path, content := range files {
		p := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum, err := mirror.FileSHA256(p)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, mirror.FileEntry{Path: path, Size: int64(len(content)), SHA256: sum})
		total += int64(len(content))
	}
	sm := mirror.StoreManifest{HFRepo: "acme/tiny", Revision: sha, TotalBytes: total, Files: entries}
	data, _ := json.Marshal(sm)
	if err := os.WriteFile(filepath.Join(root, mirror.ManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return store
}

func testManifest(storePath string) *manifest.Manifest {
	return &manifest.Manifest{
		Name:       "tiny",
		HFRepo:     "acme/tiny",
		Revision:   sha,
		S3Prefix:   storePath,
		Format:     "safetensors",
		License:    "mit",
		Runtime:    manifest.RuntimeSGLang,
		Load:       manifest.LoadNVMeCache,
		GPU:        manifest.GPU{Type: "h200", Count: 8, Nodes: 1},
		ContextMax: 4096,
		Args:       []string{"--tp-size=8"},
	}
}

var weights = map[string]string{
	"config.json":       `{"a":1}`,
	"model.safetensors": "tiny-weights",
}

func TestPrepareWeightsS3Stream(t *testing.T) {
	m := testManifest("s3://bucket/acme/tiny/" + sha + "/")
	m.Load = manifest.LoadS3Stream
	got, err := PrepareWeights(m, t.TempDir(), nil, io.Discard)
	if err != nil || got != m.S3Prefix {
		t.Fatalf("s3-stream = %q, %v", got, err)
	}
}

type countingStore struct {
	mirror.Store
	gets int
}

func (c *countingStore) Get(remote, local string) error {
	if remote != mirror.ManifestName { // manifest reads are not weight fetches
		c.gets++
	}
	return c.Store.Get(remote, local)
}

func TestPrepareWeightsCacheAndWarmSkip(t *testing.T) {
	store := &countingStore{Store: seedStore(t, weights)}
	m := testManifest(store.Store.(*mirror.LocalStore).Root)
	cache := t.TempDir()

	dir, err := PrepareWeights(m, cache, store, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "model.safetensors")); err != nil || string(data) != "tiny-weights" {
		t.Fatalf("cached weights = %q, %v", data, err)
	}
	cold := store.gets

	// Warm node: no store reads (specs/003 AC3).
	if _, err := PrepareWeights(m, cache, store, io.Discard); err != nil {
		t.Fatal(err)
	}
	if store.gets != cold {
		t.Fatalf("warm start fetched %d objects, want 0", store.gets-cold)
	}

	// Corrupted cache entry is detected and re-fetched.
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("tiny-weightX"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWeights(m, cache, store, io.Discard); err != nil {
		t.Fatal(err)
	}
	if store.gets != cold+1 {
		t.Fatalf("corruption refetch: %d gets, want 1", store.gets-cold)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "model.safetensors")); string(data) != "tiny-weights" {
		t.Fatalf("cache not repaired: %q", data)
	}
}

func TestPrepareWeightsErrors(t *testing.T) {
	// Store without a manifest → incomplete mirror.
	empty := &mirror.LocalStore{Root: t.TempDir()}
	m := testManifest(empty.Root)
	if _, err := PrepareWeights(m, t.TempDir(), empty, io.Discard); err == nil {
		t.Fatal("missing store manifest must error")
	}

	// Store serves corrupt bytes → hash mismatch after download.
	store := seedStore(t, weights)
	os.WriteFile(filepath.Join(store.Root, "model.safetensors"), []byte("evil-weights"), 0o644)
	m2 := testManifest(store.Root)
	_, err := PrepareWeights(m2, t.TempDir(), store, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("corrupt store not detected: %v", err)
	}

	// Store missing a manifest-listed file → fetch error.
	store2 := seedStore(t, weights)
	os.Remove(filepath.Join(store2.Root, "model.safetensors"))
	if _, err := PrepareWeights(testManifest(store2.Root), t.TempDir(), store2, io.Discard); err == nil {
		t.Fatal("missing store file must error")
	}

	// cacheRoot under a file → mkdir error.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	if _, err := PrepareWeights(testManifest(store2.Root), filepath.Join(blocker, "sub"), store2, io.Discard); err == nil {
		t.Fatal("bad cache root must error")
	}
}

func TestLockDirMissing(t *testing.T) {
	if _, err := lockDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("lock in missing dir must error")
	}
}

// fakeEngine is a minimal engine endpoint: /health, /metrics, /v1/*.
func fakeEngine(t *testing.T, healthy *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if *healthy {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		case "/metrics":
			fmt.Fprintln(w, "engine_requests_total 42")
		default:
			fmt.Fprintf(w, "engine:%s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestShimContract(t *testing.T) {
	healthy := false
	engine := fakeEngine(t, &healthy)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// healthz is up regardless of engine state.
	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("/healthz = %d", code)
	}
	// Not ready: weights not loaded, engine unhealthy.
	if code, _ := get("/ready"); code != 503 {
		t.Fatalf("/ready before load = %d", code)
	}
	// Weights loaded but engine still down → still 503.
	shim.SetWeightsLoaded(3 * time.Second)
	if code, _ := get("/ready"); code != 503 {
		t.Fatalf("/ready with engine down = %d", code)
	}
	healthy = true
	if code, _ := get("/ready"); code != 200 {
		t.Fatalf("/ready = %d", code)
	}
	// Metrics: engine passthrough + our gauge.
	code, body := get("/metrics")
	if code != 200 || !strings.Contains(body, "engine_requests_total 42") ||
		!strings.Contains(body, "openllms_weights_load_seconds 3") {
		t.Fatalf("/metrics = %d %q", code, body)
	}
	// Inference paths proxy through.
	if _, body := get("/v1/chat/completions"); body != "engine:/v1/chat/completions" {
		t.Fatalf("proxy body = %q", body)
	}
}

func TestShimMetricsEngineDown(t *testing.T) {
	shim, err := NewShim("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	shim.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "openllms_weights_load_seconds") {
		t.Fatalf("metrics with engine down = %d %q", rec.Code, rec.Body.String())
	}
}

func TestNewShimBadURL(t *testing.T) {
	if _, err := NewShim("://bad"); err == nil {
		t.Fatal("bad URL must error")
	}
}

func TestEngineCommand(t *testing.T) {
	m := testManifest("s3://bucket/acme/tiny/" + sha + "/")
	cmd, err := EngineCommand(m, "/models/x", 30000)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "sglang.launch_server") || !strings.Contains(joined, "--model-path /models/x") ||
		!strings.Contains(joined, "--tp-size=8") {
		t.Fatalf("sglang cmd = %q", joined)
	}

	m.Runtime = manifest.RuntimeVLLM
	cmd, _ = EngineCommand(m, "/models/x", 30000)
	if !strings.Contains(strings.Join(cmd, " "), "vllm serve /models/x") {
		t.Fatalf("vllm cmd = %q", cmd)
	}
	m.Load = manifest.LoadS3Stream
	cmd, _ = EngineCommand(m, "s3://bucket/x/", 30000)
	if !strings.Contains(strings.Join(cmd, " "), "--load-format runai_streamer") {
		t.Fatalf("s3-stream cmd = %q", cmd)
	}

	m.Runtime = manifest.RuntimeCustom
	if _, err := EngineCommand(m, "x", 1); err == nil {
		t.Fatal("custom runtime must have no engine command")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestServeE2E drives the full entrypoint: weights staged from the
// store, engine process launched, /ready flips once both are up
// (specs/003 AC1/AC2 with a fake engine standing in for the GPU one).
func TestServeE2E(t *testing.T) {
	store := seedStore(t, weights)
	m := testManifest(store.Root)

	healthy := true
	engine := fakeEngine(t, &healthy)
	engineURL, _ := url_Parse(engine.URL)
	enginePort := engineURL.Port

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
		})
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", shimPort)
	waitFor(t, base+"/ready", 200, 5*time.Second)

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "openllms_weights_load_seconds") {
		t.Fatalf("metrics missing gauge: %s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestServeEngineExit(t *testing.T) {
	store := seedStore(t, weights)
	m := testManifest(store.Root)
	err := Serve(context.Background(), m, Options{
		Port:      freePort(t),
		CacheRoot: t.TempDir(),
		EngineCmd: []string{"false"},
		Log:       io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "engine exited") {
		t.Fatalf("engine exit not reported: %v", err)
	}
}

func TestServeBadEngineStart(t *testing.T) {
	store := seedStore(t, weights)
	m := testManifest(store.Root)
	err := Serve(context.Background(), m, Options{
		Port:      freePort(t),
		CacheRoot: t.TempDir(),
		EngineCmd: []string{"/nonexistent-binary-xyz"},
		Log:       io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "start engine") {
		t.Fatalf("start failure not reported: %v", err)
	}
}

func TestServePrepFailure(t *testing.T) {
	m := testManifest(t.TempDir()) // empty store: no _manifest.json
	err := Serve(context.Background(), m, Options{
		Port:      freePort(t),
		CacheRoot: t.TempDir(),
		Log:       io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "prepare weights") {
		t.Fatalf("prep failure not reported: %v", err)
	}
}

func TestServePortConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	m := testManifest(t.TempDir())
	if err := Serve(context.Background(), m, Options{
		Port: ln.Addr().(*net.TCPAddr).Port,
		Log:  io.Discard,
	}); err == nil {
		t.Fatal("port conflict must error")
	}
}

func TestRenderOverride(t *testing.T) {
	got := renderOverride([]string{"run", "{model}", "-p", "{port}"}, "/m", 99)
	want := []string{"run", "/m", "-p", "99"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("renderOverride = %v", got)
		}
	}
}

func waitFor(t *testing.T, url string, code int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == code {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never returned %d", url, code)
}

// url_Parse extracts the port from an httptest URL.
func url_Parse(raw string) (struct{ Port int }, error) {
	var out struct{ Port int }
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	if err != nil {
		return out, err
	}
	fmt.Sscan(portStr, &out.Port)
	return out, nil
}
