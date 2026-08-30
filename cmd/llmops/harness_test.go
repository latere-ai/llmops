package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// installedHost fakes a host with one model installed and answering.
func installedHost(t *testing.T, state string) (configDir, unitDir string) {
	t.Helper()
	configDir, unitDir = t.TempDir(), t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			if state != "ready" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			_, _ = fmt.Fprintln(w, state)
		case "/metrics":
			_, _ = fmt.Fprintln(w, "llmops_weights_load_seconds 39.2")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}

	data := `name: qwen
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
	if err := os.WriteFile(filepath.Join(configDir, "qwen.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("[Service]\nExecStart=/usr/local/bin/llmops serve "+
		"--manifest /etc/llmops/qwen.yaml --port %d\n", port)
	if err := os.WriteFile(filepath.Join(unitDir, "qwen.service"), []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	return configDir, unitDir
}

func TestPSListsAndTabulates(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	var out, errw strings.Builder
	if code := run([]string{"ps", "--config-dir", cfg, "--unit-dir", units}, &out, &errw); code != 0 {
		t.Fatalf("ps exit %d: %s", code, errw.String())
	}
	for _, want := range []string{"NAME", "qwen", "ready", "vllm", "1xgb10", "39.2s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ps output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPSJSONIsMachineReadable(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	var out, errw strings.Builder
	if code := run([]string{"ps", "--json", "--config-dir", cfg, "--unit-dir", units}, &out, &errw); code != 0 {
		t.Fatalf("ps --json exit %d: %s", code, errw.String())
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("ps --json is not JSON: %v\n%s", err, out.String())
	}
	if len(got) != 1 || got[0]["Name"] != "qwen" || got[0]["State"] != "ready" {
		t.Fatalf("unexpected rows: %v", got)
	}
}

func TestPSOnAnEmptyHostSaysSo(t *testing.T) {
	var out, errw strings.Builder
	if code := run([]string{"ps", "--config-dir", t.TempDir(), "--unit-dir", t.TempDir()}, &out, &errw); code != 0 {
		t.Fatalf("ps on an empty host exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "no models installed") {
		t.Fatalf("empty host output: %q", out.String())
	}
}

// TestEndpointKeepsStdoutEvalable is specs/026 AC6: the unauthenticated
// warning must not land in output someone pipes into `eval`.
func TestEndpointKeepsStdoutEvalable(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	var out, errw strings.Builder
	code := run([]string{"endpoint", "--harness", "claude", "--format", "env",
		"--config-dir", cfg, "--unit-dir", units}, &out, &errw)
	if code != 0 {
		t.Fatalf("endpoint exit %d: %s", code, errw.String())
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, "export ") {
			t.Fatalf("stdout is not eval-able, line %q in:\n%s", line, out.String())
		}
	}
	if !strings.Contains(errw.String(), "unauthenticated") {
		t.Fatalf("no auth warning on stderr: %q", errw.String())
	}
	if !strings.Contains(out.String(), "ANTHROPIC_BASE_URL") {
		t.Fatalf("claude vars missing:\n%s", out.String())
	}
}

func TestEndpointDefaultsToWhatTheHarnessReads(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	// opencode has no env var for its base URL, so its default output
	// is the config file, not exports.
	var out, errw strings.Builder
	if code := run([]string{"endpoint", "--harness", "opencode",
		"--config-dir", cfg, "--unit-dir", units}, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out.String()), &v); err != nil {
		t.Fatalf("opencode default is not its JSON config: %v\n%s", err, out.String())
	}
	if !strings.Contains(errw.String(), "opencode.json") {
		t.Fatalf("did not say where the config goes: %q", errw.String())
	}
}

func TestEndpointWarnsWhenTheModelIsNotReady(t *testing.T) {
	cfg, units := installedHost(t, "loading")
	var out, errw strings.Builder
	if code := run([]string{"endpoint", "--harness", "claude", "--format", "env",
		"--config-dir", cfg, "--unit-dir", units}, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(errw.String(), "not ready") {
		t.Fatalf("no readiness warning: %q", errw.String())
	}
}

func TestEndpointUnknownHarnessListsKnown(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	var out, errw strings.Builder
	if code := run([]string{"endpoint", "--harness", "aider",
		"--config-dir", cfg, "--unit-dir", units}, &out, &errw); code == 0 {
		t.Fatal("unknown harness accepted")
	}
	for _, n := range []string{"claude", "codex", "opencode"} {
		if !strings.Contains(errw.String(), n) {
			t.Errorf("error does not list %q: %s", n, errw.String())
		}
	}
}

// TestRunRefusesADownModelWithTheStartCommand is specs/026 AC7: run
// must not silently start a ten-minute weight load, and must say what
// would.
func TestRunRefusesADownModelWithTheStartCommand(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	data := `name: qwen
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
	if err := os.WriteFile(filepath.Join(cfg, "qwen.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "qwen.service"),
		[]byte("[Service]\nExecStart=/usr/local/bin/llmops serve --manifest /etc/llmops/qwen.yaml --port 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errw strings.Builder
	if code := run([]string{"run", "claude", "--config-dir", cfg, "--unit-dir", units}, &out, &errw); code == 0 {
		t.Fatal("run launched a harness against a down model")
	}
	if !strings.Contains(errw.String(), "systemctl start qwen.service") {
		t.Fatalf("did not print the start command: %q", errw.String())
	}
}

func TestRunUnknownHarness(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	var out, errw strings.Builder
	if code := run([]string{"run", "aider", "--config-dir", cfg, "--unit-dir", units}, &out, &errw); code == 0 {
		t.Fatal("unknown harness accepted")
	}
}

func TestRunUsage(t *testing.T) {
	var out, errw strings.Builder
	if code := run([]string{"run"}, &out, &errw); code != 2 {
		t.Fatalf("run with no harness: exit %d, want 2", code)
	}
}

// TestRunWaitsForALoadingModel covers the case --wait exists for: a
// model already loading, which given ten-minute weight loads is the
// realistic reason to block rather than fail.
func TestRunWaitsForALoadingModel(t *testing.T) {
	cfg, units := t.TempDir(), t.TempDir()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			calls++
			if calls < 3 { // loading, then ready
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintln(w, "loading")
				return
			}
			_, _ = fmt.Fprintln(w, "ready")
		case "/metrics":
			_, _ = fmt.Fprintln(w, "llmops_weights_load_seconds 39.2")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])

	data := `name: qwen
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
	if err := os.WriteFile(filepath.Join(cfg, "qwen.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "qwen.service"), fmt.Appendf(nil,
		"[Service]\nExecStart=/usr/local/bin/llmops serve --manifest /etc/llmops/qwen.yaml --port %d\n", port), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stub the exec: reaching the real one would replace this test
	// binary with the harness, which is what happened before this hook
	// existed.
	var gotEnv []string
	defer stubExec(&gotEnv)()

	var out, errw strings.Builder
	run([]string{"run", "claude", "--wait", "20s", "--config-dir", cfg, "--unit-dir", units}, &out, &errw)
	if !slices.Contains(gotEnv, "ANTHROPIC_MODEL=qwen") {
		t.Fatalf("harness was not given the model: %v", gotEnv)
	}
	if !strings.Contains(errw.String(), "waiting up to 20s") {
		t.Fatalf("did not report waiting: %q", errw.String())
	}
	if strings.Contains(errw.String(), "did not become ready") {
		t.Fatalf("gave up on a model that became ready: %q", errw.String())
	}
}

// stubExec stands in for both steps that leave the program: the PATH
// lookup and the exec. Stubbing only one leaves the test dependent on
// whether the harness happens to be installed on the machine running it.
func stubExec(env *[]string) func() {
	prevLook, prevExec := lookHarness, execHarness
	lookHarness = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	execHarness = func(_ string, _ []string, e []string) error {
		*env = e
		return nil
	}
	return func() { lookHarness, execHarness = prevLook, prevExec }
}

// TestRunExecsWithPassthroughArgs pins that everything after -- reaches
// the harness untouched, and that the endpoint lands in its environment.
func TestRunExecsWithPassthroughArgs(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	var gotArgs, gotEnv []string
	prevLook, prevExec := lookHarness, execHarness
	lookHarness = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	execHarness = func(_ string, argv []string, e []string) error {
		gotArgs, gotEnv = argv, e
		return nil
	}
	defer func() { lookHarness, execHarness = prevLook, prevExec }()

	var out, errw strings.Builder
	if code := run([]string{"run", "claude", "--config-dir", cfg, "--unit-dir", units,
		"--", "--full-auto", "-p", "hi"}, &out, &errw); code != 0 {
		t.Fatalf("run exit %d: %s", code, errw.String())
	}
	want := []string{"claude", "--full-auto", "-p", "hi"}
	if len(gotArgs) != len(want) {
		t.Fatalf("argv %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("argv %v, want %v", gotArgs, want)
		}
	}
	for _, want := range []string{"ANTHROPIC_MODEL=qwen", "ANTHROPIC_AUTH_TOKEN=local"} {
		if !slices.Contains(gotEnv, want) {
			t.Errorf("environment missing %q", want)
		}
	}
}

// TestRunReportsAMissingHarness keeps the PATH lookup a real step: with
// both boundaries stubbed everywhere else, a harness that is genuinely
// not installed must still produce a clear error.
func TestRunReportsAMissingHarness(t *testing.T) {
	cfg, units := installedHost(t, "ready")
	prev := lookHarness
	lookHarness = func(name string) (string, error) {
		return "", fmt.Errorf("executable file not found in $PATH")
	}
	defer func() { lookHarness = prev }()

	var out, errw strings.Builder
	if code := run([]string{"run", "claude", "--config-dir", cfg, "--unit-dir", units}, &out, &errw); code == 0 {
		t.Fatal("run succeeded with the harness missing")
	}
	if !strings.Contains(errw.String(), "not on PATH") {
		t.Fatalf("error does not name the cause: %q", errw.String())
	}
}
