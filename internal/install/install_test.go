package install

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/llmops/internal/manifest"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func bareMetalManifest(t *testing.T, dir string) (*manifest.Manifest, string) {
	t.Helper()
	p := filepath.Join(dir, "qwen.yaml")
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
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestUnitNamesBinaryManifestAndServe(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())
	u := Unit(m, Options{CacheRoot: "/var/models"})
	for _, want := range []string{
		"ExecStart=" + DefaultBinPath + " serve --manifest " + DefaultConfigDir + "/qwen.yaml",
		"--cache-root /var/models",
		"TimeoutStartSec=" + DefaultStartTimeout,
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q:\n%s", want, u)
		}
	}
}

func TestUnitOmitsCacheRootWhenUnset(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())
	if strings.Contains(Unit(m, Options{}), "--cache-root") {
		t.Error("empty CacheRoot must not emit the flag")
	}
}

// TestUnitStartTimeoutOutrunsWeightLoad guards the value, not just its
// presence: systemd's 90 s default would kill a large model mid-load
// and restart it forever, which reads as a crash rather than a timeout
// (specs/019 measured 325 s for a 0.6B model).
func TestUnitStartTimeoutOutrunsWeightLoad(t *testing.T) {
	m, _ := bareMetalManifest(t, t.TempDir())
	if !strings.Contains(Unit(m, Options{}), "TimeoutStartSec=30min") {
		t.Error("start timeout is not generous enough for weight load")
	}
}

func TestRunIsIdempotent(t *testing.T) {
	src := t.TempDir()
	m, p := bareMetalManifest(t, src)
	root := t.TempDir()
	opts := Options{
		BinPath:   filepath.Join(root, "bin", "llmops"),
		ConfigDir: filepath.Join(root, "etc"),
		UnitDir:   filepath.Join(root, "units"),
	}

	first, err := Run(m, p, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ManifestChanged || !first.UnitChanged {
		t.Fatal("first install reported no writes")
	}

	// A second identical run must change nothing, so systemd is not
	// reloaded and mtimes do not churn.
	second, err := Run(m, p, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second.ManifestChanged || second.UnitChanged || second.DaemonReloaded {
		t.Fatalf("second install was not a no-op: %+v", second)
	}
}

func TestRunUpdatesChangedManifest(t *testing.T) {
	src := t.TempDir()
	m, p := bareMetalManifest(t, src)
	root := t.TempDir()
	opts := Options{
		BinPath:   filepath.Join(root, "bin", "llmops"),
		ConfigDir: filepath.Join(root, "etc"),
		UnitDir:   filepath.Join(root, "units"),
	}
	if _, err := Run(m, p, opts, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Edit the source manifest; the installed copy must follow rather
	// than a second one appearing beside it.
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "context_max: 4096", "context_max: 8192", 1)
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(m2, p, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ManifestChanged {
		t.Fatal("changed manifest was not reinstalled")
	}
	installed, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "context_max: 8192") {
		t.Fatal("installed manifest is stale")
	}
	entries, err := os.ReadDir(opts.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("install duplicated files: %d entries", len(entries))
	}
}

func TestRunRejectsK8sModel(t *testing.T) {
	// Installing a fleet model as a unit would start it outside the
	// scheduler that is supposed to own it.
	dir := t.TempDir()
	p := filepath.Join(dir, "k8s.yaml")
	data := `name: k8s
hf_repo: acme/tiny
revision: ` + sha + `
s3_prefix: s3://b/acme/tiny/` + sha + `/
format: safetensors
license: mit
runtime: sglang
load: nvme-cache
gpu: {type: h200, count: 8, nodes: 1}
context_max: 4096
args: ["--tp-size=8"]
`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(m, p, Options{ConfigDir: dir, UnitDir: dir}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "bare-metal") {
		t.Fatalf("k8s model accepted by install: %v", err)
	}
}
