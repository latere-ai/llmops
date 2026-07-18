// Package manifest defines the models/<name>.yaml schema — the single
// source of truth a deploy consumes (specs/003-serving-runtime.md).
package manifest

import (
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
// (specs/006-model-minimax-m3.md AC2: MSA needs --block-size 128).
var requiredArgs = map[string][]string{
	"MiniMaxAI/MiniMax-M3":       {"--block-size=128"},
	"MiniMaxAI/MiniMax-M3-MXFP8": {"--block-size=128"},
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
