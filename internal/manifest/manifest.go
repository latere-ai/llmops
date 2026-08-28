// Package manifest defines the models/<name>.yaml schema — the single
// source of truth a deploy consumes (specs/003-serving-runtime.md).
package manifest

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Runtimes accepted in the `runtime` field.
const (
	RuntimeSGLang = "sglang"
	RuntimeVLLM   = "vllm"
	RuntimeCustom = "custom"
)

// Load modes accepted in the `load` field.
const (
	LoadNVMeCache = "nvme-cache"
	LoadS3Stream  = "s3-stream"
)

// Deploy modes accepted in the `deploy` field (specs/020). The mode
// selects how the process is started and which deploy artifact the
// model owns, not how it serves — both modes run the same
// `runtime serve` entrypoint against the same schema.
//
// It is an explicit field rather than something inferred from gpu.type:
// deploy mode and hardware are independent axes, and inferring would
// silently swap a model's deploy artifact when its GPU changed.
const (
	DeployK8s       = "k8s"        // container + deploy/<name>/lws.yaml
	DeployBareMetal = "bare-metal" // installed binary + systemd unit
)

// GPU describes the resource shape a model requires.
type GPU struct {
	Type  string `yaml:"type"`
	Count int    `yaml:"count"`
	Nodes int    `yaml:"nodes"`
}

// SystemPrompt modes.
const (
	SystemPromptDefault  = "default"  // used only when the caller sends none
	SystemPromptPrepend  = "prepend"  // always inserted before the caller's
	SystemPromptOverride = "override" // replaces whatever the caller sent
)

// SystemPrompt is an inference-layer-owned system prompt the shim
// enforces on every request (specs/003). Per-tenant/policy prompts
// belong in Lux, not here.
type SystemPrompt struct {
	Mode string `yaml:"mode"`
	Text string `yaml:"text"`
}

// Manifest is one models/<name>.yaml.
type Manifest struct {
	Name         string        `yaml:"name"`
	HFRepo       string        `yaml:"hf_repo"`
	Revision     string        `yaml:"revision"`
	S3Prefix     string        `yaml:"s3_prefix"`
	Format       string        `yaml:"format"`
	License      string        `yaml:"license"`
	LicenseNote  string        `yaml:"license_note,omitempty"`
	Runtime      string        `yaml:"runtime"`
	Image        string        `yaml:"image,omitempty"`
	Deploy       string        `yaml:"deploy,omitempty"`
	Load         string        `yaml:"load"`
	GPU          GPU           `yaml:"gpu"`
	ContextMax   int           `yaml:"context_max"`
	Args         []string      `yaml:"args,omitempty"`
	SystemPrompt *SystemPrompt `yaml:"system_prompt,omitempty"`
}

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	revisionRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hfRepoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// requiredArgs pins flags a model cannot legally run without
// (specs/006-model-minimax-m3.md AC2: MSA needs --block-size 128;
// specs/017 AC2 and specs/018 AC2: both checkpoints ship custom modeling
// code via auto_map and never load without --trust-remote-code).
var requiredArgs = map[string][]string{
	"MiniMaxAI/MiniMax-M3":               {"--block-size=128"},
	"MiniMaxAI/MiniMax-M3-MXFP8":         {"--block-size=128"},
	"moonshotai/Kimi-K3":                 {"--trust-remote-code"},
	"deepseek-ai/DeepSeek-V4-Flash-0731": {"--trust-remote-code"},
}

// specDSpark is the value of --speculative-algorithm that selects the
// in-checkpoint DSpark draft head (specs/017).
const specDSpark = "DSPARK"

// engineImages overrides the default image name for models the engine's
// shared image cannot serve. Kimi-K3 support ships in a CUDA 13 branch
// build with an r580+ driver floor, so the K3 image and the shared
// SGLang image are not substitutable in either direction (specs/018 AC4).
var engineImages = map[string]string{
	"moonshotai/Kimi-K3": "open-llms-runtime-sglang-k3",
}

// EngineImage is the image name a deploy must reference for this model,
// without registry prefix or tag. Empty for runtime: custom, where the
// manifest's own Image field is the contract.
func (m *Manifest) EngineImage() string {
	if m.Runtime == RuntimeCustom {
		return ""
	}
	if img, ok := engineImages[m.HFRepo]; ok {
		return img
	}
	return "open-llms-runtime-" + m.Runtime
}

// DeployMode is the model's deploy mode, defaulting to k8s so every
// manifest written before the field existed keeps its meaning.
func (m *Manifest) DeployMode() string {
	return cmp.Or(m.Deploy, DeployK8s)
}

// DeployArtifact is the path, relative to the deploy directory, of the
// one artifact this model owns. A model never has both: the artifacts
// describe the same thing in two mechanisms, so a second one is a
// second source of truth (specs/020).
func (m *Manifest) DeployArtifact() string {
	if m.DeployMode() == DeployBareMetal {
		return filepath.Join(m.Name, m.Name+".service")
	}
	return filepath.Join(m.Name, "lws.yaml")
}

// Parse decodes a manifest, rejecting unknown fields.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Load reads and validates a manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// LoadDir validates every *.yaml in dir and returns them sorted by name.
func LoadDir(dir string) ([]*Manifest, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []*Manifest
	for _, p := range paths {
		m, err := Load(p)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Validate enforces the schema rules from specs/003-serving-runtime.md.
func (m *Manifest) Validate() error {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if !nameRe.MatchString(m.Name) {
		fail("name %q must match %s", m.Name, nameRe)
	}
	if !hfRepoRe.MatchString(m.HFRepo) {
		fail("hf_repo %q must be <org>/<name>", m.HFRepo)
	}
	if !revisionRe.MatchString(m.Revision) {
		fail("revision %q must be a pinned 40-hex commit SHA", m.Revision)
	}
	if err := m.validateS3Prefix(); err != nil {
		fail("%v", err)
	}
	if m.Format == "" {
		fail("format is required")
	}
	if m.License == "" {
		fail("license is required")
	}

	switch m.Runtime {
	case RuntimeSGLang, RuntimeVLLM:
		if m.Image != "" {
			fail("image is only allowed with runtime: custom")
		}
		if len(m.Args) == 0 {
			fail("args are required for engine runtimes")
		}
	case RuntimeCustom:
		if m.Image == "" {
			fail("runtime: custom requires image")
		}
	default:
		fail("runtime %q must be one of sglang|vllm|custom", m.Runtime)
	}

	switch m.DeployMode() {
	case DeployK8s:
	case DeployBareMetal:
		// No container is built or pulled in this mode, so an image
		// reference would name something that never gets deployed.
		if m.Image != "" {
			fail("deploy: bare-metal has no container, so image is not allowed")
		}
	default:
		fail("deploy %q must be one of k8s|bare-metal", m.Deploy)
	}

	switch m.Load {
	case LoadNVMeCache:
	case LoadS3Stream:
		if m.Runtime != RuntimeVLLM {
			fail("load: s3-stream is vllm-only (got runtime %q)", m.Runtime)
		}
	default:
		fail("load %q must be one of nvme-cache|s3-stream", m.Load)
	}

	if m.GPU.Type == "" || m.GPU.Count <= 0 || m.GPU.Nodes <= 0 {
		fail("gpu requires type, count>0, nodes>0 (got %+v)", m.GPU)
	}
	if m.ContextMax <= 0 {
		fail("context_max must be positive")
	}
	for _, req := range requiredArgs[m.HFRepo] {
		if !m.HasArg(req) {
			fail("hf_repo %s requires arg %s", m.HFRepo, req)
		}
	}
	m.validateDSpark(fail)
	if sp := m.SystemPrompt; sp != nil {
		switch sp.Mode {
		case SystemPromptDefault, SystemPromptPrepend, SystemPromptOverride:
		default:
			fail("system_prompt.mode %q must be one of default|prepend|override", sp.Mode)
		}
		if strings.TrimSpace(sp.Text) == "" {
			fail("system_prompt.text must be non-empty")
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid manifest: %s", strings.Join(errs, "; "))
	}
	return nil
}

// validateS3Prefix checks the prefix is revision-pinned and matches the
// HF repo: s3://<bucket>/<hf_repo>/<revision>/ (specs/002).
func (m *Manifest) validateS3Prefix() error {
	rest, ok := strings.CutPrefix(m.S3Prefix, "s3://")
	if !ok {
		return fmt.Errorf("s3_prefix %q must start with s3://", m.S3Prefix)
	}
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" {
		return fmt.Errorf("s3_prefix %q must include a bucket", m.S3Prefix)
	}
	want := m.HFRepo + "/" + m.Revision + "/"
	if key != want {
		return fmt.Errorf("s3_prefix key %q must be %q (hf_repo/revision/)", key, want)
	}
	return nil
}

// validateDSpark enforces the constraints SGLang places on the
// in-checkpoint DSpark draft head (specs/017). Each one is silent at
// launch and wrong at serve time, so the manifest is the place to catch
// them: the draft weights come from the target checkpoint (no separate
// path), and the algorithm requires pp_size == 1 with DP attention off.
func (m *Manifest) validateDSpark(fail func(string, ...any)) {
	if v, ok := m.FlagValue("--speculative-algorithm"); !ok || v != specDSpark {
		return
	}
	if _, ok := m.FlagValue("--speculative-draft-model-path"); ok {
		fail("--speculative-algorithm %s draws its draft head from the target checkpoint; drop --speculative-draft-model-path", specDSpark)
	}
	if v, ok := m.FlagValue("--pp-size"); ok && v != "1" {
		fail("--speculative-algorithm %s requires --pp-size 1 (got %q)", specDSpark, v)
	}
	if _, ok := m.FlagValue("--enable-dp-attention"); ok {
		fail("--speculative-algorithm %s is incompatible with --enable-dp-attention", specDSpark)
	}
}

// FlagValue returns the value args give to flag and whether flag is
// present at all. Both "--k=v" and "--k v" forms resolve; a valueless
// flag ("--k", or "--k" followed by another flag) reports "", true.
func (m *Manifest) FlagValue(flag string) (string, bool) {
	for i, a := range m.Args {
		if k, v, ok := strings.Cut(a, "="); ok {
			if k == flag {
				return v, true
			}
			continue
		}
		if a != flag {
			continue
		}
		if i+1 < len(m.Args) && !strings.HasPrefix(m.Args[i+1], "-") {
			return m.Args[i+1], true
		}
		return "", true
	}
	return "", false
}

// HasArg reports whether args contain flag either as "--k=v" or "--k v".
func (m *Manifest) HasArg(flag string) bool {
	k, v, hasVal := strings.Cut(flag, "=")
	for i, a := range m.Args {
		if a == flag {
			return true
		}
		if hasVal && a == k && i+1 < len(m.Args) && m.Args[i+1] == v {
			return true
		}
	}
	return false
}
