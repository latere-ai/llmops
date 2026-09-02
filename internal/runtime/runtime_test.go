// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/mirror"

	"latere.ai/x/pkg/wait/waittest"
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
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
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

// seedLocal lays weights out the way a bare-metal host holds them:
// <root>/<hf_repo>/<revision>/, frozen with its own _manifest.json.
func seedLocal(t *testing.T, m *manifest.Manifest) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, m.HFRepo, m.Revision)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := seedStore(t, weights)
	for name := range weights {
		if err := src.Get(name, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.Get(mirror.ManifestName, filepath.Join(dir, mirror.ManifestName)); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func TestPrepareWeightsLocalVerifiesInPlace(t *testing.T) {
	m := testManifest("")
	m.Load = manifest.LoadLocal
	root, dir := seedLocal(t, m)

	// A nil store proves nothing is fetched: any store call would panic.
	got, err := PrepareWeights(m, root, nil, io.Discard)
	if err != nil {
		t.Fatalf("local prepare: %v", err)
	}
	if got != dir {
		t.Fatalf("local prepare = %q, want %q", got, dir)
	}

	// Nothing is copied into the store beyond the lock file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "model.safetensors", "config.json", mirror.ManifestName, ".lock":
		default:
			t.Errorf("unexpected file written into the local store: %s", e.Name())
		}
	}
}

func TestPrepareWeightsLocalFailsOnCorruption(t *testing.T) {
	// nvme-cache re-fetches a bad file. A local store has no upstream,
	// so the launch must fail rather than silently serve bad weights.
	m := testManifest("")
	m.Load = manifest.LoadLocal
	root, dir := seedLocal(t, m)
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("corrupted!!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWeights(m, root, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("corrupt local store accepted: %v", err)
	}
}

func TestPrepareWeightsLocalMissingDir(t *testing.T) {
	m := testManifest("")
	m.Load = manifest.LoadLocal
	if _, err := PrepareWeights(m, t.TempDir(), nil, io.Discard); err == nil {
		t.Fatal("missing local weights accepted")
	}
}

func TestExpandHome(t *testing.T) {
	// systemd does not expand ~ in ExecStart, so the runtime must.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/.models"); got != filepath.Join(home, ".models") {
		t.Errorf("expandHome(~/.models) = %q", got)
	}
	if got := expandHome("/cache"); got != "/cache" {
		t.Errorf("absolute path rewritten to %q", got)
	}
	// A path merely starting with the character is not a home path.
	if got := expandHome("~models/x"); got != "~models/x" {
		t.Errorf("expandHome(~models/x) = %q", got)
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
	if err := os.WriteFile(filepath.Join(store.Root, "model.safetensors"), []byte("evil-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := testManifest(store.Root)
	_, err := PrepareWeights(m2, t.TempDir(), store, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("corrupt store not detected: %v", err)
	}

	// Store missing a manifest-listed file → fetch error.
	store2 := seedStore(t, weights)
	if err := os.Remove(filepath.Join(store2.Root, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWeights(testManifest(store2.Root), t.TempDir(), store2, io.Discard); err == nil {
		t.Fatal("missing store file must error")
	}

	// cacheRoot under a file → mkdir error.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
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
			_, _ = fmt.Fprintln(w, "engine_requests_total 42")
		default:
			_, _ = fmt.Fprintf(w, "engine:%s", r.URL.Path)
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
		defer func() { _ = resp.Body.Close() }()
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
		!strings.Contains(body, "llmops_weights_load_seconds 3") {
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
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "llmops_weights_load_seconds") {
		t.Fatalf("metrics with engine down = %d %q", rec.Code, rec.Body.String())
	}
}

func TestShimMetricsOmitsEngineErrorBody(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "<html>503 Service Unavailable</html>")
	}))
	defer engine.Close()

	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	shim.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "llmops_weights_load_seconds") {
		t.Fatalf("runtime gauge missing: %d %q", rec.Code, body)
	}
	if strings.Contains(body, "<html>") || strings.Contains(body, "503 Service Unavailable") {
		t.Fatalf("engine error body exposed as metrics: %d %q", rec.Code, body)
	}
}

func TestNewShimBadURL(t *testing.T) {
	if _, err := NewShim("://bad"); err == nil {
		t.Fatal("bad URL must error")
	}
}

func TestEngineCommand(t *testing.T) {
	m := testManifest("s3://bucket/acme/tiny/" + sha + "/")
	cmd, err := EngineCommand(m, "/models/x", 30000, manifest.Speculation{Name: manifest.SpeculatorNone}, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "sglang.launch_server") || !strings.Contains(joined, "--model-path /models/x") ||
		!strings.Contains(joined, "--tp-size=8") || !strings.Contains(joined, "--served-model-name tiny") {
		t.Fatalf("sglang cmd = %q", joined)
	}

	m.Runtime = manifest.RuntimeVLLM
	cmd, _ = EngineCommand(m, "/models/x", 30000, manifest.Speculation{Name: manifest.SpeculatorNone}, "")
	if !strings.Contains(strings.Join(cmd, " "), "vllm serve /models/x") {
		t.Fatalf("vllm cmd = %q", cmd)
	}
	m.Load = manifest.LoadS3Stream
	cmd, _ = EngineCommand(m, "s3://bucket/x/", 30000, manifest.Speculation{Name: manifest.SpeculatorNone}, "")
	if !strings.Contains(strings.Join(cmd, " "), "--load-format runai_streamer") {
		t.Fatalf("s3-stream cmd = %q", cmd)
	}

	m.Runtime = manifest.RuntimeCustom
	if _, err := EngineCommand(m, "x", 1, manifest.Speculation{Name: manifest.SpeculatorNone}, ""); err == nil {
		t.Fatal("custom runtime must have no engine command")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
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
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "llmops_weights_load_seconds") {
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
	defer func() { _ = ln.Close() }()
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
	waittest.For(t, timeout, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == code
	})
}

// chatEngine fakes the engine's OpenAI Chat surface for dialect tests.
func chatEngine(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if fail, _ := req["model"].(string); fail == "boom" {
			http.Error(w, `{"error":"kaput"}`, http.StatusInternalServerError)
			return
		}
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, chunk := range []string{
				`{"id":"c1","object":"chat.completion.chunk","model":"tiny","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`,
				`{"id":"c1","object":"chat.completion.chunk","model":"tiny","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
				`{"id":"c1","object":"chat.completion.chunk","model":"tiny","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			} {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
				fl.Flush()
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestShimAnthropicMessages(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), `"message"`) || !strings.Contains(string(out), "hello") {
		t.Fatalf("not an anthropic message: %s", out)
	}
}

func TestShimAnthropicMessagesStream(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	s := string(out)
	if !strings.Contains(s, "message_start") || !strings.Contains(s, "content_block_delta") ||
		!strings.Contains(s, "message_stop") {
		t.Fatalf("not an anthropic SSE stream: %s", s)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
}

func TestShimAnthropicMessagesErrors(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	// Malformed request body.
	resp, _ := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader("{"))
	if resp.StatusCode != 400 {
		t.Fatalf("bad body status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Engine-side failure passes its status through.
	body := `{"model":"boom","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	resp, _ = http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if resp.StatusCode != 500 {
		t.Fatalf("engine error status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Wrong method.
	resp, _ = http.Get(front.URL + "/v1/messages")
	if resp.StatusCode != 405 {
		t.Fatalf("GET status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Engine unreachable.
	dead, _ := NewShim("http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"tiny","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	dead.ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Fatalf("dead engine status %d", rec.Code)
	}
}

func TestShimAnthropicMessagesBadUpstream(t *testing.T) {
	// Engine returns 200 with garbage — non-stream and stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {not-json\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "definitely-not-json")
	}))
	defer srv.Close()
	shim, err := NewShim(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	resp, _ := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if resp.StatusCode != 502 {
		t.Fatalf("garbage upstream status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Streaming garbage: status is already 200, but the handler must
	// terminate without panicking.
	sbody := `{"model":"tiny","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err = http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(sbody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("read boom") }

func TestShimAnthropicMessagesBodyReadError(t *testing.T) {
	shim, err := NewShim("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", errReader{})
	shim.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("body read error status %d", rec.Code)
	}
}

// recordingEngine captures the chat request body the engine receives.
func recordingEngine(t *testing.T, got *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = body
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func roles(t *testing.T, body []byte) []string {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("engine body not json: %v: %s", err, body)
	}
	var out []string
	for _, m := range req["messages"].([]any) {
		mm := m.(map[string]any)
		out = append(out, fmt.Sprint(mm["role"], ":", mm["content"]))
	}
	return out
}

func TestSystemPromptInjection(t *testing.T) {
	const withSystem = `{"model":"tiny","messages":[{"role":"system","content":"caller"},{"role":"user","content":"hi"}]}`
	const noSystem = `{"model":"tiny","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		mode, body string
		want       []string
	}{
		{"default", noSystem, []string{"system:ours", "user:hi"}},
		{"default", withSystem, []string{"system:caller", "user:hi"}},
		{"prepend", withSystem, []string{"system:ours", "system:caller", "user:hi"}},
		{"override", withSystem, []string{"system:ours", "user:hi"}},
		{"override", noSystem, []string{"system:ours", "user:hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.body[:20], func(t *testing.T) {
			var got []byte
			engine := recordingEngine(t, &got)
			shim, err := NewShim(engine.URL)
			if err != nil {
				t.Fatal(err)
			}
			shim.SystemPrompt = &manifest.SystemPrompt{Mode: tc.mode, Text: "ours"}
			front := httptest.NewServer(shim)
			defer front.Close()

			resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			gotRoles := roles(t, got)
			if fmt.Sprint(gotRoles) != fmt.Sprint(tc.want) {
				t.Fatalf("messages = %v, want %v", gotRoles, tc.want)
			}
		})
	}
}

func TestForwardPreservesCallerHeadersAndQuery(t *testing.T) {
	var gotAuth, gotAccept, gotTrace, gotQuery string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotTrace = r.Header.Get("X-Trace-Id")
		gotQuery = r.URL.RawQuery
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"c1","choices":[]}`)
	}))
	defer engine.Close()

	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	shim.SystemPrompt = &manifest.SystemPrompt{Mode: "override", Text: "ours"}
	front := httptest.NewServer(shim)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost,
		front.URL+"/v1/chat/completions?trace=abc123",
		strings.NewReader(`{"model":"tiny","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-token")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Trace-Id", "trace-xyz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotAuth != "Bearer caller-token" {
		t.Errorf("engine Authorization = %q, want caller token", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("engine Accept = %q, want text/event-stream", gotAccept)
	}
	if gotTrace != "trace-xyz" {
		t.Errorf("engine X-Trace-Id = %q, want trace-xyz", gotTrace)
	}
	if gotQuery != "trace=abc123" {
		t.Errorf("engine query = %q, want trace=abc123", gotQuery)
	}
}

func TestSystemPromptInjectionAnthropicPath(t *testing.T) {
	var got []byte
	engine := recordingEngine(t, &got)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	shim.SystemPrompt = &manifest.SystemPrompt{Mode: "override", Text: "ours"}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","max_tokens":8,"system":"caller","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	gotRoles := roles(t, got)
	if fmt.Sprint(gotRoles) != "[system:ours user:hi]" {
		t.Fatalf("messages = %v", gotRoles)
	}
}

func TestAnthropicForwardPreservesCallerHeadersAndQuery(t *testing.T) {
	var gotAuth, gotTrace, gotQuery string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTrace = r.Header.Get("X-Trace-Id")
		gotQuery = r.URL.RawQuery
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer engine.Close()

	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages?trace=abc123",
		strings.NewReader(`{"model":"tiny","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-token")
	req.Header.Set("X-Trace-Id", "trace-xyz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotAuth != "Bearer caller-token" || gotTrace != "trace-xyz" || gotQuery != "trace=abc123" {
		t.Fatalf("engine metadata = auth %q, trace %q, query %q", gotAuth, gotTrace, gotQuery)
	}
}

func TestSystemPromptPassthroughWhenUnset(t *testing.T) {
	// Without a system prompt the transparent proxy serves the OpenAI
	// path (covered by TestShimContract); here: injection helper is a
	// no-op on nil.
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := injectSystemPrompt(body, nil)
	if err != nil || string(out) != string(body) {
		t.Fatalf("nil prompt must be identity: %s, %v", out, err)
	}
}

func TestSystemPromptInjectionErrors(t *testing.T) {
	sp := &manifest.SystemPrompt{Mode: "override", Text: "x"}
	if _, err := injectSystemPrompt([]byte("{"), sp); err == nil {
		t.Fatal("bad json must error")
	}
	if _, err := injectSystemPrompt([]byte("{}"), &manifest.SystemPrompt{Mode: "bogus", Text: "x"}); err == nil {
		t.Fatal("bad mode must error")
	}
	// Handler surfaces the 400.
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	shim.SystemPrompt = sp
	rec := httptest.NewRecorder()
	shim.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{")))
	if rec.Code != 400 {
		t.Fatalf("bad body status %d", rec.Code)
	}
	// GET with prompt set still proxies (only POST is intercepted).
	rec = httptest.NewRecorder()
	shim.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/chat/completions", nil))
	if rec.Code == 400 {
		t.Fatal("GET must not be intercepted")
	}
	// Engine down → 502.
	dead, _ := NewShim("http://127.0.0.1:1")
	dead.SystemPrompt = sp
	rec = httptest.NewRecorder()
	dead.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 502 {
		t.Fatalf("dead engine status %d", rec.Code)
	}
}

func TestSystemPromptStreamingThroughForward(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	shim.SystemPrompt = &manifest.SystemPrompt{Mode: "default", Text: "ours"}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "chat.completion.chunk") || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("stream not passed through: %s", out)
	}
}

func TestShimHealthPathOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/custom-health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	shim, err := NewShim(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if shim.EngineHealthy(t.Context()) {
		t.Fatal("default /health should be 404 here")
	}
	shim.HealthPath = "/custom-health"
	if !shim.EngineHealthy(t.Context()) {
		t.Fatal("custom health path not used")
	}
}

// captureEngine records the OpenAI Chat request the shim forwards upstream and
// answers a minimal completion, so a test can assert on what the engine is
// actually asked rather than only on what the caller gets back.
func captureEngine(t *testing.T, into *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read forwarded body: %v", err)
		}
		*into = b
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A user turn may interleave text and tool results in one content array. The
// engine has no array-content equivalent for a tool result -- it needs a
// separate message with role "tool" -- so the translator has to split the turn.
// The order it splits into is what the model reads as the conversation.
//
// This pins that the split preserves the caller's authored order. An earlier
// dialect version hoisted the tool message ahead of text that was authored
// before it, which told the model a tool had answered before a question the
// caller had in fact asked first. The shim has no test that exercises a
// tool_result at all, so nothing else here would notice that regressing.
func TestShimAnthropicToolResultPreservesTurnOrder(t *testing.T) {
	var forwarded []byte
	engine := captureEngine(t, &forwarded)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","max_tokens":32,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"before"},` +
		`{"type":"tool_result","tool_use_id":"tu_1","content":"R"},` +
		`{"type":"text","text":"after"}]}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}

	var sent struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(forwarded, &sent); err != nil {
		t.Fatalf("decode forwarded request: %v\n%s", err, forwarded)
	}

	want := []struct {
		role, content, toolCallID string
	}{
		{"user", "before", ""},
		{"tool", "R", "tu_1"},
		{"user", "after", ""},
	}
	if len(sent.Messages) != len(want) {
		t.Fatalf("forwarded %d messages, want %d:\n%s", len(sent.Messages), len(want), forwarded)
	}
	for i, w := range want {
		got := sent.Messages[i]
		if got.Role != w.role {
			t.Errorf("message %d role = %q, want %q", i, got.Role, w.role)
		}
		if s, _ := got.Content.(string); s != w.content {
			t.Errorf("message %d content = %v, want %q", i, got.Content, w.content)
		}
		if got.ToolCallID != w.toolCallID {
			t.Errorf("message %d tool_call_id = %q, want %q", i, got.ToolCallID, w.toolCallID)
		}
	}
}

// TestShimServesEveryCallerDialect covers the surfaces the shim gained
// in specs/025: three caller dialects on one engine, not one.
func TestShimServesEveryCallerDialect(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"tiny","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/messages", `{"model":"tiny","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"tiny","input":"hi"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Post(front.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			out, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			if !bytes.Contains(out, []byte("hello")) {
				t.Fatalf("answer did not survive translation: %s", out)
			}
		})
	}
}

// TestShimNativeSurfaceIsNotRoundTripped pins that a caller speaking the
// engine's own dialect is proxied rather than translated. Sending it
// through the IR and back would cost a round trip and lose whatever the
// IR cannot carry, for nothing (specs/025 AC3).
func TestShimNativeSurfaceIsNotRoundTripped(t *testing.T) {
	var got []byte
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}))
	defer engine.Close()
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	// A field the IR does not model at all: it must survive untouched,
	// which only happens if the body was never decoded.
	sent := `{"model":"tiny","messages":[{"role":"user","content":"hi"}],"x_vendor_passthrough":{"keep":1}}`
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if string(got) != sent {
		t.Fatalf("native surface was rewritten:\n got  %s\n want %s", got, sent)
	}
}

// TestShimReportsDialectLoss is the criterion the old code silently
// failed: llmdialect collects fields a target dialect cannot carry, and
// the shim discarded the report (specs/025 AC4).
func TestShimReportsDialectLoss(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	// logprobs has no member in Anthropic Messages, so asking for it
	// through that surface is reported as loss rather than ignored.
	body := `{"model":"tiny","max_tokens":32,"top_logprobs":3,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	loss := resp.Header.Get(LossHeader)
	if loss == "" {
		t.Fatalf("no %s header; the loss report was discarded again", LossHeader)
	}

	// The same loss must be countable by an operator, not only visible
	// to the one caller who hit it.
	m := httptest.NewRecorder()
	shim.ServeHTTP(m, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(m.Body.String(), "llmops_dialect_loss_total") {
		t.Fatalf("loss not exported as a metric:\n%s", m.Body.String())
	}
}

// TestShimTranslatedSurfaceRejectsNonPost pins that a surface only this
// shim serves claims its own methods: proxying a GET would 404 from the
// engine and blame the wrong component.
func TestShimTranslatedSurfaceRejectsNonPost(t *testing.T) {
	engine := chatEngine(t)
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a translated surface: %d, want 405", resp.StatusCode)
	}
}

// TestShimEngineDialectSelectsBackend pins that the engine's dialect,
// not an assumption, decides what gets translated and where it is sent.
func TestShimEngineDialectSelectsBackend(t *testing.T) {
	var hitPath string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"tiny",`+
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer engine.Close()
	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	// An engine that speaks Anthropic: now the OpenAI surface is the
	// translated one and /v1/messages is native.
	if err := shim.setDialect(manifest.EngineDialectAnthropic); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"tiny","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if hitPath != "/v1/messages" {
		t.Fatalf("engine was hit at %q, want /v1/messages", hitPath)
	}
}
