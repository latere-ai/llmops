// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package manifest defines the models/<name>.yaml schema — the single
// source of truth a deploy consumes (specs/003-serving-runtime.md).
package manifest

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// LoadLocal serves weights already on the host's disk, verified in
	// place against _manifest.json and never copied (specs/021). The
	// freeze guarantee is a pinned revision plus per-file checksums,
	// not the storage behind them, so it survives having no bucket.
	LoadLocal = "local"
)

// Engine dialects accepted in the `engine_dialect` field (specs/025):
// the wire dialect the *engine* speaks, which the shim translates
// callers into. Values match latere.ai/x/pkg/llmdialect's ir.Dialect so
// the two cannot drift.
//
// Declared rather than inferred from `runtime`, because it is a property
// of the engine build and not of our choice of engine. SGLang and vLLM
// both serve OpenAI Chat today; an engine serving something else needs a
// way to say so, and assuming would translate a caller's dialect into
// one the engine then translates back.
const (
	EngineDialectOpenAIChat      = "openai-chat"
	EngineDialectAnthropic       = "anthropic-messages"
	EngineDialectOpenAIResponses = "openai-responses"
)

// Deploy modes accepted in the `deploy` field (specs/020). The mode
// selects how the process is started and which deploy artifact the
// model owns, not how it serves — both modes run the same
// `llmops serve` entrypoint against the same schema.
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

// GPUTypeGB10 is the single-GPU unified-memory class (specs/019). CPU
// and GPU share one 128 GB pool, so an engine's memory fraction is
// taken out of the host's memory as well as the accelerator's.
const GPUTypeGB10 = "gb10"

// gb10MaxMemFraction caps the engine's share of the unified pool.
// 0.80 x 128 GB leaves the host roughly 26 GB. Measured 2026-08-28:
// vLLM at 0.30 held 37.6 GB, confirming the fraction is applied to the
// whole pool and that the engine fills whatever it is given.
const gb10MaxMemFraction = 0.80

// Per-engine names for the two flags a gb10 manifest must state: the
// share of the unified pool, and the KV cache bound. Both engines
// otherwise supply a default sized for discrete HBM.
var (
	memFractionFlags = map[string]string{
		RuntimeSGLang: "--mem-fraction-static",
		RuntimeVLLM:   "--gpu-memory-utilization",
	}
	contextFlags = map[string]string{
		RuntimeSGLang: "--context-length",
		RuntimeVLLM:   "--max-model-len",
	}
)

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

// SpeculatorNone selects no speculative decoding. It is a reserved name
// rather than an empty string so that turning speculation off is
// something a caller states, and so `--speculator none` on the command
// line reads the same as `default_speculator: none` in the file.
//
// Serving without speculation is a measurement the fast path needs, not
// an edge case: it separates what quantization costs in quality from
// what speculation adds in throughput (specs/027).
const SpeculatorNone = "none"

// Speculator is one draft-model configuration a manifest offers.
//
// Which speculative algorithm wins is workload-dependent — on this
// class DSpark leads on code and DFlash2 on prose, by a margin wide
// enough that choosing one at author time would be choosing a workload
// on the operator's behalf. So a manifest offers a set, and the choice
// is made when the model is started (specs/027).
//
// A draft head lives in one of two places. Some checkpoints carry their
// own, in which case Args alone select it (specs/017). Others are
// separately published repos, and those are frozen and verified exactly
// like primary weights: pinned revision, own license. That second kind
// is why this is a struct rather than a list of flags — a third-party
// head under a different license than the base model is not something
// an argument string can state.
type Speculator struct {
	HFRepo      string   `yaml:"hf_repo,omitempty"`
	Revision    string   `yaml:"revision,omitempty"`
	S3Prefix    string   `yaml:"s3_prefix,omitempty"`
	License     string   `yaml:"license,omitempty"`
	LicenseNote string   `yaml:"license_note,omitempty"`
	Args        []string `yaml:"args"`
}

// SeparateHead reports whether the draft weights are a repo of their
// own rather than part of the target checkpoint. It decides whether
// serving has a second artifact to prepare and verify.
func (s Speculator) SeparateHead() bool { return s.HFRepo != "" }

// Manifest is one models/<name>.yaml.
type Manifest struct {
	Name          string        `yaml:"name"`
	HFRepo        string        `yaml:"hf_repo"`
	Revision      string        `yaml:"revision"`
	S3Prefix      string        `yaml:"s3_prefix"`
	Format        string        `yaml:"format"`
	License       string        `yaml:"license"`
	LicenseNote   string        `yaml:"license_note,omitempty"`
	Runtime       string        `yaml:"runtime"`
	Image         string        `yaml:"image,omitempty"`
	Deploy        string        `yaml:"deploy,omitempty"`
	EngineDialect string        `yaml:"engine_dialect,omitempty"`
	Load          string        `yaml:"load"`
	GPU           GPU           `yaml:"gpu"`
	ContextMax    int           `yaml:"context_max"`
	Args          []string      `yaml:"args,omitempty"`
	SystemPrompt  *SystemPrompt `yaml:"system_prompt,omitempty"`

	// Speculators are the draft-model configurations this model offers,
	// keyed by the name `llmops serve --speculator` selects.
	Speculators map[string]Speculator `yaml:"speculators,omitempty"`
	// DefaultSpeculator is the one used when the operator names none.
	// Required whenever Speculators is non-empty, and allowed to be
	// SpeculatorNone: which draft head runs by default changes both
	// throughput and output, so it is never left implicit.
	DefaultSpeculator string `yaml:"default_speculator,omitempty"`
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
	"moonshotai/Kimi-K3": "llmops-runtime-sglang-k3",
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
	return "llmops-runtime-" + m.Runtime
}

// Dialect is the wire dialect the engine speaks, defaulting to OpenAI
// Chat — true of both engines we ship, and of every manifest written
// before the field existed (specs/025).
func (m *Manifest) Dialect() string {
	return cmp.Or(m.EngineDialect, EngineDialectOpenAIChat)
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
	if err := m.validateWeightSource(); err != nil {
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

	switch m.Dialect() {
	case EngineDialectOpenAIChat, EngineDialectAnthropic, EngineDialectOpenAIResponses:
	default:
		fail("engine_dialect %q must be one of openai-chat|anthropic-messages|openai-responses", m.EngineDialect)
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
	case LoadLocal:
	default:
		fail("load %q must be one of nvme-cache|s3-stream|local", m.Load)
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
	m.validateGB10(fail)
	m.validateSpeculators(fail)
	m.validateSpeculation(fail)
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

// validateWeightSource checks the manifest names its weights exactly
// once (specs/021).
//
// `load: local` names no source at all: the weights live at
// <weights-root>/<hf_repo>/<revision>, the same layout nvme-cache uses,
// with the root supplied by the host at serve time. Keeping the root
// out of the manifest is what lets one checked-in manifest describe a
// model on any machine — an absolute path here would pin the file to
// one host's directory layout.
func (m *Manifest) validateWeightSource() error {
	if m.Load == LoadLocal {
		if m.S3Prefix != "" {
			return fmt.Errorf("load: local reads from the weights root, so s3_prefix must be empty")
		}
		return nil
	}
	return m.validateS3Prefix()
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

// validateGB10 enforces what unified memory changes (specs/019). On a
// discrete-HBM node an unset memory fraction costs the engine some
// throughput; here it costs the operating system its memory, because
// the fraction is taken from the one pool both share. Each rule below
// is silent at launch and fatal to the host at serve time, so the
// manifest is where they have to be caught.
func (m *Manifest) validateGB10(fail func(string, ...any)) {
	if m.GPU.Type != GPUTypeGB10 {
		return
	}
	if m.GPU.Count != 1 || m.GPU.Nodes != 1 {
		fail("gpu.type %s is a single-GPU host: count and nodes must both be 1 (got count=%d, nodes=%d)",
			GPUTypeGB10, m.GPU.Count, m.GPU.Nodes)
	}

	// runtime: custom launches itself, so it owns its own limits.
	frac, ok := memFractionFlags[m.Runtime]
	if !ok {
		return
	}
	// The cap is checked against every way this model can be started,
	// not just its base arguments. A speculator's flags are appended
	// after the model's own and therefore win, so one raising the
	// fraction past the ceiling would otherwise pass validation and
	// starve the host only when that speculator was selected.
	checkFraction(m.Args, frac, "", fail)
	for _, name := range m.SpeculatorNames() {
		checkFraction(append(slices.Clone(m.Args), m.Speculators[name].Args...), frac, name, fail)
	}

	// Presence only, and deliberately against the base args alone: the
	// context bound belongs to the model, not to a draft head. A
	// speculator supplying one the model omitted would still fail here,
	// which is the safe direction — unlike the fraction above, no
	// speculator can weaken this by overriding it.
	if ctx := contextFlags[m.Runtime]; ctx != "" {
		if _, present := m.FlagValue(ctx); !present {
			fail("gpu.type %s requires %s so the kv cache size is stated rather than inherited from an engine default",
				GPUTypeGB10, ctx)
		}
	}
}

// checkFraction enforces the unified-memory ceiling on one combination
// of arguments (specs/019).
func checkFraction(args []string, frac, spec string, fail func(string, ...any)) {
	where := ""
	if spec != "" {
		where = fmt.Sprintf("speculator %q: ", spec)
	}
	switch v, present := flagValue(args, frac); {
	case !present:
		fail("%sgpu.type %s requires %s: memory is unified, so the engine default is taken out of the host's memory too",
			where, GPUTypeGB10, frac)
	case v == "":
		fail("%s%s requires a value", where, frac)
	default:
		f, err := strconv.ParseFloat(v, 64)
		switch {
		case err != nil:
			fail("%s%s %q is not a number", where, frac, v)
		case f <= 0:
			fail("%s%s=%s must be positive", where, frac, v)
		case f > gb10MaxMemFraction:
			fail("%s%s=%s exceeds %.2f on gpu.type %s: the fraction applies to the whole unified pool, so this starves the host",
				where, frac, v, gb10MaxMemFraction, GPUTypeGB10)
		}
	}
}

// validateSpeculators checks the speculator set itself: that a default
// is stated, that every entry adds arguments, and that an entry naming
// its own draft repo pins and licenses it as strictly as the primary
// weights are pinned and licensed.
//
// The license rule is the one with teeth. A third-party draft head can
// carry a different license than the base model — the published DSpark
// head for this model does — and a manifest that omits it would ship
// that artifact under the base model's declaration (specs/027).
func (m *Manifest) validateSpeculators(fail func(string, ...any)) {
	if len(m.Speculators) == 0 {
		if m.DefaultSpeculator != "" && m.DefaultSpeculator != SpeculatorNone {
			fail("default_speculator %q names nothing: no speculators are declared", m.DefaultSpeculator)
		}
		return
	}
	switch d := m.DefaultSpeculator; d {
	case "":
		fail("default_speculator is required when speculators are declared: which draft head runs changes both throughput and output, so it is never implicit (choose one of %s, or %q)",
			strings.Join(m.SpeculatorNames(), ", "), SpeculatorNone)
	case SpeculatorNone:
	default:
		if _, ok := m.Speculators[d]; !ok {
			fail("default_speculator %q is not a declared speculator (have %s)", d, strings.Join(m.SpeculatorNames(), ", "))
		}
	}

	for _, name := range m.SpeculatorNames() {
		sp := m.Speculators[name]
		switch {
		case name == SpeculatorNone:
			fail("speculator %q is reserved for serving without speculation", SpeculatorNone)
		case !nameRe.MatchString(name):
			fail("speculator name %q must match %s", name, nameRe)
		}
		if len(sp.Args) == 0 {
			fail("speculator %q must state args: an entry that adds no flags selects nothing", name)
		}
		if !sp.SeparateHead() {
			// An in-checkpoint head is covered by the model's own
			// revision and license, so restating them here would be a
			// second source of truth for the same artifact.
			switch {
			case sp.Revision != "":
				fail("speculator %q sets revision without hf_repo: an in-checkpoint head is pinned by the model's own revision", name)
			case sp.License != "":
				fail("speculator %q sets license without hf_repo: an in-checkpoint head carries the model's own license", name)
			case sp.S3Prefix != "":
				fail("speculator %q sets s3_prefix without hf_repo", name)
			}
			continue
		}
		if !hfRepoRe.MatchString(sp.HFRepo) {
			fail("speculator %q hf_repo %q must be <org>/<name>", name, sp.HFRepo)
		}
		if !revisionRe.MatchString(sp.Revision) {
			fail("speculator %q revision %q must be a pinned 40-hex commit SHA", name, sp.Revision)
		}
		if sp.License == "" {
			fail("speculator %q must state license: a separately published draft head is its own artifact and may not share the base model's terms", name)
		}
		if err := sp.validateWeightSource(m.Load); err != nil {
			fail("speculator %q: %v", name, err)
		}
	}
}

// validateWeightSource applies the model's load mode to a draft head.
//
// s3-stream is rejected rather than inherited: the engine takes a draft
// checkpoint as a filesystem path, so a head that is only in a bucket
// has nothing to hand it. Such a model must stage its head with
// nvme-cache even though its primary weights stream.
func (s Speculator) validateWeightSource(load string) error {
	switch load {
	case LoadLocal:
		if s.S3Prefix != "" {
			return fmt.Errorf("load: local reads from the weights root, so s3_prefix must be empty")
		}
		return nil
	case LoadS3Stream:
		return fmt.Errorf("a draft head is loaded from a path, not streamed; give it s3_prefix and serve the model with load: nvme-cache")
	}
	rest, ok := strings.CutPrefix(s.S3Prefix, "s3://")
	if !ok {
		return fmt.Errorf("s3_prefix %q must start with s3://", s.S3Prefix)
	}
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" {
		return fmt.Errorf("s3_prefix %q must include a bucket", s.S3Prefix)
	}
	if want := s.HFRepo + "/" + s.Revision + "/"; key != want {
		return fmt.Errorf("s3_prefix key %q must be %q (hf_repo/revision/)", key, want)
	}
	return nil
}

// draftPathFlag is the SGLang flag naming a separate draft checkpoint.
// llmops always supplies it, never the manifest — see validateDSpark.
const draftPathFlag = "--speculative-draft-model-path"

// validateSpeculation checks every way this model can be started: its
// bare arguments, and its arguments combined with each speculator it
// offers. A rule that held for the default and broke for one speculator
// would surface only when someone selected that one.
func (m *Manifest) validateSpeculation(fail func(string, ...any)) {
	validateDSpark(m.Args, false, "", fail)
	for _, name := range m.SpeculatorNames() {
		sp := m.Speculators[name]
		validateDSpark(append(slices.Clone(m.Args), sp.Args...), sp.SeparateHead(), name, fail)
	}
}

// validateDSpark enforces the constraints SGLang places on the DSpark
// algorithm. Each is silent at launch and wrong at serve time, so the
// manifest is where they have to be caught: the algorithm requires
// pp_size == 1 with DP attention off, and the draft weights must come
// from wherever the manifest says they do and nowhere else.
//
// Where the head lives is a property of the checkpoint, not of the
// algorithm. DeepSeek publishes one inside the target checkpoint, so
// there is no path to give (specs/017); the Qwen head is a separately
// published repo, so there is (specs/027). Both are DSpark. Keying this
// rule on the algorithm name alone is what made the first version of it
// reject the second case.
//
// Either way the manifest never writes the path itself. It resolves to
// <cache-root>/<hf_repo>/<revision>, and the root belongs to the host —
// the same reason primary weights name no directory (specs/021).
func validateDSpark(args []string, separateHead bool, spec string, fail func(string, ...any)) {
	if v, ok := flagValue(args, "--speculative-algorithm"); !ok || v != specDSpark {
		return
	}
	where := ""
	if spec != "" {
		where = fmt.Sprintf("speculator %q: ", spec)
	}
	if _, ok := flagValue(args, draftPathFlag); ok {
		if separateHead {
			fail("%s%s is derived from the speculator's hf_repo at serve time; drop it", where, draftPathFlag)
		} else {
			fail("%s--speculative-algorithm %s draws its draft head from the target checkpoint; drop %s",
				where, specDSpark, draftPathFlag)
		}
	}
	if v, ok := flagValue(args, "--pp-size"); ok && v != "1" {
		fail("%s--speculative-algorithm %s requires --pp-size 1 (got %q)", where, specDSpark, v)
	}
	if _, ok := flagValue(args, "--enable-dp-attention"); ok {
		fail("%s--speculative-algorithm %s is incompatible with --enable-dp-attention", where, specDSpark)
	}
}

// SpeculatorNames lists the speculators this model offers, sorted.
func (m *Manifest) SpeculatorNames() []string {
	names := make([]string, 0, len(m.Speculators))
	for n := range m.Speculators {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Speculation is the resolved draft-model choice for one serve: the
// name to report, the arguments to add, and the artifact to prepare.
type Speculation struct {
	Name       string // SpeculatorNone when speculation is off
	Speculator Speculator
}

// Off reports whether this resolves to serving without speculation.
func (s Speculation) Off() bool { return s.Name == SpeculatorNone }

// ResolveSpeculator turns the operator's `--speculator` choice into the
// configuration to serve. An empty name takes the manifest's default,
// so the flag is optional and the file still decides.
func (m *Manifest) ResolveSpeculator(name string) (Speculation, error) {
	if name == "" {
		name = cmp.Or(m.DefaultSpeculator, SpeculatorNone)
	}
	if name == SpeculatorNone {
		return Speculation{Name: SpeculatorNone}, nil
	}
	sp, ok := m.Speculators[name]
	if !ok {
		if len(m.Speculators) == 0 {
			return Speculation{}, fmt.Errorf("model %s offers no speculators, so --speculator must be %q", m.Name, SpeculatorNone)
		}
		return Speculation{}, fmt.Errorf("model %s has no speculator %q: choose one of %s, or %q",
			m.Name, name, strings.Join(m.SpeculatorNames(), ", "), SpeculatorNone)
	}
	return Speculation{Name: name, Speculator: sp}, nil
}

// FlagValue returns the value the manifest's own args give to flag.
func (m *Manifest) FlagValue(flag string) (string, bool) {
	return flagValue(m.Args, flag)
}

// flagValue returns the value args give to flag and whether flag is
// present at all. Both "--k=v" and "--k v" forms resolve; a valueless
// flag ("--k", or "--k" followed by another flag) reports "", true.
//
// A repeated flag resolves to its *last* occurrence, because that is
// what both engines do with one. It matters as soon as arguments are
// composed: a speculator's flags are appended after the model's own
// precisely so they can override, so reading the first occurrence would
// validate a value the engine is never going to use.
func flagValue(args []string, flag string) (string, bool) {
	value, found := "", false
	for i, a := range args {
		if k, v, ok := strings.Cut(a, "="); ok {
			if k == flag {
				value, found = v, true
			}
			continue
		}
		if a != flag {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value, found = args[i+1], true
			continue
		}
		value, found = "", true
	}
	return value, found
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
