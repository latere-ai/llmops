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

func TestShimMetricsOmitsEngineErrorBody(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "<html>503 Service Unavailable</html>")
	}))
	defer engine.Close()

	shim, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	shim.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "openllms_weights_load_seconds") {
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
	cmd, err := EngineCommand(m, "/models/x", 30000)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "sglang.launch_server") || !strings.Contains(joined, "--model-path /models/x") ||
		!strings.Contains(joined, "--tp-size=8") || !strings.Contains(joined, "--served-model-name tiny") {
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

// chatEngine fakes the engine's OpenAI Chat surface for dialect tests.
func chatEngine(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
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
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
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
	resp, err := http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
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
	resp, err := http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
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
	resp, _ := http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader("{"))
	if resp.StatusCode != 400 {
		t.Fatalf("bad body status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Engine-side failure passes its status through.
	body := `{"model":"boom","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	resp, _ = http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if resp.StatusCode != 500 {
		t.Fatalf("engine error status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong method.
	resp, _ = http.Get(front.URL + "/anthropic/v1/messages")
	if resp.StatusCode != 405 {
		t.Fatalf("GET status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Engine unreachable.
	dead, _ := NewShim("http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(
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
		json.NewDecoder(r.Body).Decode(&req)
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {not-json\n\n")
			return
		}
		fmt.Fprint(w, "definitely-not-json")
	}))
	defer srv.Close()
	shim, err := NewShim(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(shim)
	defer front.Close()

	body := `{"model":"tiny","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	resp, _ := http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if resp.StatusCode != 502 {
		t.Fatalf("garbage upstream status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Streaming garbage: status is already 200, but the handler must
	// terminate without panicking.
	sbody := `{"model":"tiny","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err = http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(sbody))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("read boom") }

func TestShimAnthropicMessagesBodyReadError(t *testing.T) {
	shim, err := NewShim("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", errReader{})
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
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"tiny","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
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
			resp.Body.Close()
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
	resp, err := http.Post(front.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	gotRoles := roles(t, got)
	if fmt.Sprint(gotRoles) != "[system:ours user:hi]" {
		t.Fatalf("messages = %v", gotRoles)
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
	defer resp.Body.Close()
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
	if shim.EngineHealthy() {
		t.Fatal("default /health should be 404 here")
	}
	shim.HealthPath = "/custom-health"
	if !shim.EngineHealthy() {
		t.Fatal("custom health path not used")
	}
}
