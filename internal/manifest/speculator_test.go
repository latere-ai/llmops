package manifest

import (
	"strings"
	"testing"
)

const draftSHA = "9f2c1ab4e5d60783b1c2f4a9e8d70b6c5a4f3e21"

// fastManifest is the shape specs/027 introduces: quantized primary
// weights on a single unified-memory box, offering two separately
// published draft heads and the option of neither.
func fastManifest() *Manifest {
	return &Manifest{
		Name:       "qwen3.8-27b-fast",
		HFRepo:     "RadixArk/Qwen3.8-27B-NVFP4-BF16-LMHead",
		Revision:   sha,
		Format:     "nvfp4",
		License:    "apache-2.0",
		Runtime:    RuntimeSGLang,
		Deploy:     DeployBareMetal,
		Load:       LoadLocal,
		GPU:        GPU{Type: GPUTypeGB10, Count: 1, Nodes: 1},
		ContextMax: 262144,
		Args:       []string{"--mem-fraction-static=0.80", "--context-length=262144"},
		Speculators: map[string]Speculator{
			"dspark": {
				HFRepo:      "RadixArk/Qwen3.8-27B-DSpark",
				Revision:    draftSHA,
				License:     "other",
				LicenseNote: "third-party draft head for an apache-2.0 base",
				Args:        []string{"--speculative-algorithm=DSPARK", "--speculative-num-draft-tokens=8"},
			},
			"dflash2": {
				HFRepo:   "z-lab/Qwen3.8-27B-DFlash2",
				Revision: draftSHA,
				License:  "apache-2.0",
				Args:     []string{"--speculative-algorithm=DFLASH", "--speculative-num-draft-tokens=8"},
			},
		},
		DefaultSpeculator: "dspark",
	}
}

func TestFastManifestValidates(t *testing.T) {
	if err := fastManifest().Validate(); err != nil {
		t.Fatalf("speculator manifest rejected: %v", err)
	}
}

// TestDSparkWithASeparateHeadIsLegal is the regression this design
// exists for. The first DSpark rule fired on any manifest naming the
// algorithm and assumed the head was in the target checkpoint, which is
// true of specs/017 and false of specs/027 — so the Qwen fast path
// could not be expressed at all.
func TestDSparkWithASeparateHeadIsLegal(t *testing.T) {
	m := fastManifest()
	sp := m.Speculators["dspark"]
	if !sp.SeparateHead() {
		t.Fatal("a speculator naming an hf_repo must report a separate head")
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("DSpark with a separately published head rejected: %v", err)
	}

	// And the in-checkpoint guarantee still holds: specs/017's manifest
	// has no speculators block, so its head comes from the target and
	// naming a path is still an error.
	f := flashManifest()
	f.Args = append(f.Args, "--speculative-draft-model-path=/cache/draft")
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "target checkpoint") {
		t.Fatalf("in-checkpoint DSpark no longer rejects a draft path: %v", err)
	}
}

// TestSpeculatorRulesApplyToTheCombination catches a constraint that
// holds for the base args and breaks only once a speculator is added.
// Validating the two separately would let that ship.
func TestSpeculatorRulesApplyToTheCombination(t *testing.T) {
	m := fastManifest()
	m.Args = append(m.Args, "--pp-size=2") // legal alone, illegal with DSpark
	err := m.Validate()
	if err == nil {
		t.Fatal("pp-size 2 combined with a DSpark speculator was accepted")
	}
	for _, want := range []string{`speculator "dspark"`, "--pp-size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestManifestNeverWritesTheDraftPath: the path depends on the host's
// cache root, so llmops derives it. A manifest stating one would pin
// the file to a single machine, the same reason primary weights name no
// directory (specs/021).
func TestManifestNeverWritesTheDraftPath(t *testing.T) {
	m := fastManifest()
	sp := m.Speculators["dspark"]
	sp.Args = append(sp.Args, "--speculative-draft-model-path=/home/someone/.models/draft")
	m.Speculators["dspark"] = sp
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "derived from the speculator's hf_repo") {
		t.Fatalf("a hand-written draft path was accepted: %v", err)
	}
}

func TestSpeculatorValidationRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Manifest)
		want string
	}{
		{"no default stated", func(m *Manifest) { m.DefaultSpeculator = "" }, "default_speculator is required"},
		{"default names nothing", func(m *Manifest) { m.DefaultSpeculator = "eagle" }, "not a declared speculator"},
		{"default without any speculators", func(m *Manifest) {
			m.Speculators = nil
			m.DefaultSpeculator = "dspark"
		}, "names nothing"},
		{"reserved name", func(m *Manifest) {
			m.Speculators[SpeculatorNone] = Speculator{Args: []string{"--x"}}
		}, "reserved"},
		{"bad name", func(m *Manifest) {
			m.Speculators["Fast Path"] = Speculator{Args: []string{"--x"}}
		}, "must match"},
		{"no args", func(m *Manifest) {
			sp := m.Speculators["dspark"]
			sp.Args = nil
			m.Speculators["dspark"] = sp
		}, "must state args"},
		{"unpinned draft revision", func(m *Manifest) {
			sp := m.Speculators["dspark"]
			sp.Revision = "main"
			m.Speculators["dspark"] = sp
		}, "pinned 40-hex"},
		{"bad draft repo", func(m *Manifest) {
			sp := m.Speculators["dspark"]
			sp.HFRepo = "nope"
			m.Speculators["dspark"] = sp
		}, "<org>/<name>"},
		{"draft head with no license", func(m *Manifest) {
			sp := m.Speculators["dspark"]
			sp.License = ""
			m.Speculators["dspark"] = sp
		}, "must state license"},
		{"local model with a bucket draft", func(m *Manifest) {
			sp := m.Speculators["dspark"]
			sp.S3Prefix = "s3://b/RadixArk/Qwen3.8-27B-DSpark/" + draftSHA + "/"
			m.Speculators["dspark"] = sp
		}, "s3_prefix must be empty"},
		{"revision without a repo", func(m *Manifest) {
			m.Speculators["inchk"] = Speculator{Revision: draftSHA, Args: []string{"--speculative-algorithm=X"}}
		}, "pinned by the model's own revision"},
		{"license without a repo", func(m *Manifest) {
			m.Speculators["inchk"] = Speculator{License: "mit", Args: []string{"--speculative-algorithm=X"}}
		}, "carries the model's own license"},
		{"prefix without a repo", func(m *Manifest) {
			m.Speculators["inchk"] = Speculator{S3Prefix: "s3://b/x/y/", Args: []string{"--x"}}
		}, "s3_prefix without hf_repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fastManifest()
			tc.mut(m)
			err := m.Validate()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestDraftHeadStagedForAStreamingModel: a draft checkpoint is opened
// from a path, so a model whose primary weights stream from S3 still
// has to stage its head somewhere. Saying so is better than silently
// passing the engine a URI it cannot open.
func TestDraftHeadStagedForAStreamingModel(t *testing.T) {
	m := fastManifest()
	m.Runtime = RuntimeVLLM
	m.Args = []string{"--gpu-memory-utilization=0.80", "--max-model-len=262144"}
	m.Load = LoadS3Stream
	m.S3Prefix = "s3://b/" + m.HFRepo + "/" + sha + "/"
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "loaded from a path, not streamed") {
		t.Fatalf("s3-stream with a draft head was accepted: %v", err)
	}

	// nvme-cache is the supported combination, and the head's prefix is
	// checked exactly as the model's own prefix is.
	m.Load = LoadNVMeCache
	sp := m.Speculators["dspark"]
	sp.S3Prefix = "s3://b/RadixArk/Qwen3.8-27B-DSpark/" + draftSHA + "/"
	m.Speculators["dspark"] = sp
	delete(m.Speculators, "dflash2")
	if err := m.Validate(); err != nil {
		t.Fatalf("nvme-cache draft head rejected: %v", err)
	}

	sp.S3Prefix = "s3://b/wrong/repo/" + draftSHA + "/"
	m.Speculators["dspark"] = sp
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "hf_repo/revision") {
		t.Fatalf("a draft prefix naming the wrong repo was accepted: %v", err)
	}
	sp.S3Prefix = "gs://b/x/"
	m.Speculators["dspark"] = sp
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "s3://") {
		t.Fatalf("a non-s3 draft prefix was accepted: %v", err)
	}
	sp.S3Prefix = "s3://"
	m.Speculators["dspark"] = sp
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("a bucketless draft prefix was accepted: %v", err)
	}
}

func TestResolveSpeculator(t *testing.T) {
	m := fastManifest()

	// No choice takes the manifest's default.
	got, err := m.ResolveSpeculator("")
	if err != nil || got.Name != "dspark" {
		t.Fatalf("default = %q, %v; want dspark", got.Name, err)
	}
	if got.Off() {
		t.Fatal("the default speculator reports itself as off")
	}
	if len(got.Speculator.Args) == 0 {
		t.Fatal("resolved speculator carries no args")
	}

	// A named one wins over the default.
	got, err = m.ResolveSpeculator("dflash2")
	if err != nil || got.Name != "dflash2" {
		t.Fatalf("dflash2 = %q, %v", got.Name, err)
	}

	// "none" is a real choice, not an absent one: it is how the fast
	// path is measured without speculation (specs/027 AC4).
	got, err = m.ResolveSpeculator(SpeculatorNone)
	if err != nil || !got.Off() || len(got.Speculator.Args) != 0 {
		t.Fatalf("none = %+v, %v", got, err)
	}

	// An unknown name lists what is on offer rather than failing bare.
	_, err = m.ResolveSpeculator("eagle")
	if err == nil {
		t.Fatal("unknown speculator accepted")
	}
	for _, want := range []string{"dflash2", "dspark", SpeculatorNone} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not offer %q", err, want)
		}
	}
}

func TestResolveSpeculatorWhenNoneAreOffered(t *testing.T) {
	m := valid()
	got, err := m.ResolveSpeculator("")
	if err != nil || !got.Off() {
		t.Fatalf("a manifest with no speculators must resolve to none: %+v, %v", got, err)
	}
	_, err = m.ResolveSpeculator("dspark")
	if err == nil || !strings.Contains(err.Error(), "offers no speculators") {
		t.Fatalf("error = %v", err)
	}
}

func TestDefaultSpeculatorMayBeNone(t *testing.T) {
	// A model can offer draft heads and still default to serving
	// without one — the honest default when the quality cost of
	// speculation has not been measured yet.
	m := fastManifest()
	m.DefaultSpeculator = SpeculatorNone
	if err := m.Validate(); err != nil {
		t.Fatalf("default_speculator: none rejected: %v", err)
	}
	got, err := m.ResolveSpeculator("")
	if err != nil || !got.Off() {
		t.Fatalf("resolved %+v, %v", got, err)
	}
}

func TestSpeculatorNamesAreSorted(t *testing.T) {
	got := fastManifest().SpeculatorNames()
	want := []string{"dflash2", "dspark"}
	if len(got) != len(want) {
		t.Fatalf("names = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
}

// TestSpeculatorsParseFromYAML pins the wire shape operators write.
func TestSpeculatorsParseFromYAML(t *testing.T) {
	const doc = `
name: qwen3.8-27b-fast
hf_repo: RadixArk/Qwen3.8-27B-NVFP4-BF16-LMHead
revision: ` + sha + `
format: nvfp4
license: apache-2.0
runtime: sglang
deploy: bare-metal
load: local
gpu: { type: gb10, count: 1, nodes: 1 }
context_max: 262144
args:
  - --mem-fraction-static=0.80
  - --context-length=262144
default_speculator: dspark
speculators:
  dspark:
    hf_repo: RadixArk/Qwen3.8-27B-DSpark
    revision: ` + draftSHA + `
    license: other
    license_note: third-party head
    args: [--speculative-algorithm=DSPARK]
`
	m, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	sp, ok := m.Speculators["dspark"]
	if !ok {
		t.Fatal("dspark did not parse")
	}
	if sp.HFRepo != "RadixArk/Qwen3.8-27B-DSpark" || sp.License != "other" ||
		sp.LicenseNote != "third-party head" || sp.Revision != draftSHA {
		t.Fatalf("speculator = %+v", sp)
	}
}
