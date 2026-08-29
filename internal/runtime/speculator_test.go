package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/mirror"
)

const draftSHA = "9f2c1ab4e5d60783b1c2f4a9e8d70b6c5a4f3e21"

func draftHead() manifest.Speculator {
	return manifest.Speculator{
		HFRepo:   "acme/tiny-draft",
		Revision: draftSHA,
		License:  "other",
		Args:     []string{"--speculative-algorithm=DSPARK", "--speculative-num-draft-tokens=8"},
	}
}

// TestEngineCommandCarriesTheSpeculator: the speculator's flags come
// after the model's own, so several speculators share one base
// configuration instead of each restating it.
func TestEngineCommandCarriesTheSpeculator(t *testing.T) {
	m := testManifest("s3://bucket/acme/tiny/" + sha + "/")
	spec := manifest.Speculation{Name: "dspark", Speculator: draftHead()}

	cmd, err := EngineCommand(m, "/models/x", 30000, spec, "/cache/acme/tiny-draft/"+draftSHA)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{
		"--speculative-algorithm=DSPARK",
		"--speculative-num-draft-tokens=8",
		"--speculative-draft-model-path=/cache/acme/tiny-draft/" + draftSHA,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("engine command missing %q:\n%s", want, joined)
		}
	}
	// The base configuration survives.
	if !strings.Contains(joined, "--tp-size=8") || !strings.Contains(joined, "--served-model-name tiny") {
		t.Errorf("speculator flags displaced the base args:\n%s", joined)
	}
	if i, j := strings.Index(joined, "--tp-size=8"), strings.Index(joined, "--speculative-algorithm"); i > j {
		t.Error("speculator flags must come after the model's own so they can override")
	}
}

// TestEngineCommandWithoutASpeculator is what `--speculator none`
// produces: the quantized weights alone, which is the measurement that
// separates quality cost from throughput gain (specs/027).
func TestEngineCommandWithoutASpeculator(t *testing.T) {
	m := testManifest("s3://bucket/acme/tiny/" + sha + "/")
	cmd, err := EngineCommand(m, "/models/x", 30000, manifest.Speculation{Name: manifest.SpeculatorNone}, "")
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(cmd, " "); strings.Contains(joined, "speculative") {
		t.Fatalf("speculator none still passed speculative flags:\n%s", joined)
	}
}

// TestEngineCommandInCheckpointHeadHasNoPath: a head that lives in the
// target checkpoint is selected by flags alone, so nothing is appended.
func TestEngineCommandInCheckpointHeadHasNoPath(t *testing.T) {
	m := testManifest("s3://bucket/acme/tiny/" + sha + "/")
	spec := manifest.Speculation{
		Name:       "dspark",
		Speculator: manifest.Speculator{Args: []string{"--speculative-algorithm=DSPARK"}},
	}
	cmd, err := EngineCommand(m, "/models/x", 30000, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "--speculative-algorithm=DSPARK") {
		t.Fatalf("algorithm flag missing:\n%s", joined)
	}
	if strings.Contains(joined, "--speculative-draft-model-path") {
		t.Fatalf("a path was invented for an in-checkpoint head:\n%s", joined)
	}
}

// seedDraft lays a draft head out under the same cache layout the
// primary weights use.
func seedDraft(t *testing.T, root string, sp manifest.Speculator) string {
	t.Helper()
	dir := filepath.Join(root, sp.HFRepo, sp.Revision)
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
	return dir
}

// TestPrepareDraftVerifiesLikeAnyOtherArtifact is the point of giving a
// speculator its own hf_repo and revision: the freeze guarantee extends
// to the draft head without new machinery.
func TestPrepareDraftVerifiesLikeAnyOtherArtifact(t *testing.T) {
	root := t.TempDir()
	sp := draftHead()
	dir := seedDraft(t, root, sp)

	// A nil store proves the local path fetches nothing.
	got, err := PrepareDraft(sp, manifest.LoadLocal, root, nil, io.Discard)
	if err != nil || got != dir {
		t.Fatalf("PrepareDraft = %q, %v; want %q", got, err, dir)
	}

	// A corrupted head fails the launch rather than serving quietly with
	// a draft model nobody pinned.
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDraft(sp, manifest.LoadLocal, root, nil, io.Discard); err == nil {
		t.Fatal("a corrupted draft head was accepted")
	}
}

func TestPrepareDraftInCheckpointStagesNothing(t *testing.T) {
	got, err := PrepareDraft(manifest.Speculator{Args: []string{"--x"}}, manifest.LoadLocal, t.TempDir(), nil, io.Discard)
	if err != nil || got != "" {
		t.Fatalf("in-checkpoint head = %q, %v; want no path", got, err)
	}
}

// TestPrepareDraftStagesEvenWhenTheModelStreams: the engine opens a
// draft checkpoint from a path, so it is fetched to disk even though
// the primary weights are streamed from the bucket.
func TestPrepareDraftStagesEvenWhenTheModelStreams(t *testing.T) {
	root := t.TempDir()
	sp := draftHead()
	sp.S3Prefix = "s3://bucket/acme/tiny-draft/" + draftSHA + "/"
	store := seedStore(t, weights)

	got, err := PrepareDraft(sp, manifest.LoadS3Stream, root, store, io.Discard)
	if err != nil {
		t.Fatalf("PrepareDraft: %v", err)
	}
	want := filepath.Join(root, sp.HFRepo, sp.Revision)
	if got != want {
		t.Fatalf("PrepareDraft = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, "model.safetensors")); err != nil {
		t.Fatalf("draft head was not staged: %v", err)
	}
}

// TestSpeculatorIsReportedOnEveryResponse: which draft head answered
// changes throughput and can change the tokens, so a benchmark that
// cannot see it cannot attribute its own numbers (specs/027 AC3).
func TestSpeculatorIsReportedOnEveryResponse(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer engine.Close()

	s, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Speculator = "dspark"

	// A proxied path: the reverse proxy adds the engine's headers to
	// what is already set rather than replacing them.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := rec.Header().Get(SpeculatorHeader); got != "dspark" {
		t.Errorf("/v1/models %s = %q", SpeculatorHeader, got)
	}

	// And a path the shim answers itself.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Header().Get(SpeculatorHeader); got != "dspark" {
		t.Errorf("/healthz %s = %q", SpeculatorHeader, got)
	}

	// A shim serving no model configuration says nothing rather than
	// claiming speculation is off.
	s.Speculator = ""
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Header().Get(SpeculatorHeader); got != "" {
		t.Errorf("unset speculator reported %q", got)
	}
}

func TestSpeculatorMetric(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer engine.Close()

	s, err := NewShim(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Speculator = manifest.SpeculatorNone

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `llmops_speculator_info{speculator="none"} 1`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("metrics missing %q:\n%s", want, rec.Body.String())
	}
}
