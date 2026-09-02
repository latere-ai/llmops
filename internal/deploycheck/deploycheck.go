// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package deploycheck cross-validates deploy/ manifests against
// models/ manifests (specs/008-k8s-serving.md AC1: rendering/consistency
// is a CI gate, GPU serving is a release gate).
package deploycheck

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/latere-ai/llmops/internal/manifest"
)

// DNSName converts a model name to its k8s resource name.
func DNSName(model string) string { return strings.ReplaceAll(model, ".", "-") }

// Binary is the installed command a bare-metal unit must run
// (specs/024). A unit naming anything else would start something that
// is not this repo's runtime, or nothing at all.
const Binary = "llmops"

// Validate checks every model manifest against the deploy artifact it
// owns: a LeaderWorkerSet for deploy: k8s, a systemd unit for
// deploy: bare-metal (specs/020).
//
// The check dispatches on the model's mode so neither mode is validated
// against the other's artifact, and a model is required to own exactly
// one — two would be two sources of truth for how it starts.
func Validate(modelsDir, deployDir string) error {
	models, err := manifest.LoadDir(modelsDir)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no model manifests in %s", modelsDir)
	}
	for _, m := range models {
		path := filepath.Join(deployDir, m.DeployArtifact())
		if err := validateArtifact(m, path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := checkNoStrayArtifact(m, deployDir); err != nil {
			return err
		}
	}
	return nil
}

func validateArtifact(m *manifest.Manifest, path string) error {
	if m.DeployMode() == manifest.DeployBareMetal {
		return validateUnit(m, path)
	}
	return validateOne(m, path)
}

// checkNoStrayArtifact rejects a model that carries both a LWS manifest
// and a unit file. Whichever one the mode does not select would go
// unchecked, and an unchecked deploy artifact is how a manifest and a
// running process drift apart without CI noticing.
func checkNoStrayArtifact(m *manifest.Manifest, deployDir string) error {
	other := filepath.Join(deployDir, m.Name, "lws.yaml")
	if m.DeployMode() == manifest.DeployK8s {
		other = filepath.Join(deployDir, m.Name, m.Name+".service")
	}
	if _, err := os.Stat(other); err == nil {
		return fmt.Errorf("%s: %s also has %s; a model owns one deploy artifact, not both",
			m.Name, m.DeployMode(), filepath.Base(other))
	}
	return nil
}

// validateUnit checks a systemd unit starts this model, with this
// binary, from this manifest (specs/020 AC5, specs/024 AC7).
func validateUnit(m *manifest.Manifest, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var exec string
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			exec = rest
			break
		}
	}
	if exec == "" {
		return errors.New("no ExecStart= line")
	}
	fields := strings.Fields(exec)
	if len(fields) == 0 {
		return errors.New("empty ExecStart")
	}
	if got := filepath.Base(fields[0]); got != Binary {
		return fmt.Errorf("ExecStart runs %q, want %q", got, Binary)
	}
	if !slices.Contains(fields[1:], "serve") {
		return fmt.Errorf("ExecStart does not run the serve subcommand: %q", exec)
	}
	wantManifest := m.Name + ".yaml"
	var gotManifest string
	for i, f := range fields {
		if f == "--manifest" && i+1 < len(fields) {
			gotManifest = fields[i+1]
		}
		if rest, ok := strings.CutPrefix(f, "--manifest="); ok {
			gotManifest = rest
		}
	}
	if gotManifest == "" {
		return errors.New("ExecStart has no --manifest")
	}
	if filepath.Base(gotManifest) != wantManifest {
		return fmt.Errorf("ExecStart serves manifest %q, want %q", gotManifest, wantManifest)
	}
	return nil
}

func validateOne(m *manifest.Manifest, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	docs, err := splitDocs(data)
	if err != nil {
		return err
	}
	var lws map[string]any
	for _, d := range docs {
		if kind, _ := dig(d, "kind").(string); kind == "LeaderWorkerSet" {
			lws = d
		}
	}
	if lws == nil {
		return fmt.Errorf("no LeaderWorkerSet document")
	}

	if name, _ := dig(lws, "metadata", "name").(string); name != DNSName(m.Name) {
		return fmt.Errorf("metadata.name %q, want %q", name, DNSName(m.Name))
	}
	if size, _ := dig(lws, "spec", "leaderWorkerTemplate", "size").(int); size != m.GPU.Nodes {
		return fmt.Errorf("leaderWorkerTemplate.size %v, want gpu.nodes %d", size, m.GPU.Nodes)
	}

	if err := validateContainer(m, lws, "leaderTemplate", true); err != nil {
		return err
	}
	// Above one node, LWS runs the extra nodes from workerTemplate. Its
	// container carries the same engine image and the same per-node GPU
	// count as the leader's — an unchecked worker is a rank that either
	// never joins the group or joins with the wrong weights
	// (specs/018 AC3). Probes are leader-only: only rank 0 serves HTTP,
	// so the worker has no /ready to answer.
	if m.GPU.Nodes > 1 {
		if dig(lws, "spec", "leaderWorkerTemplate", "workerTemplate") == nil {
			return fmt.Errorf("gpu.nodes %d requires a workerTemplate", m.GPU.Nodes)
		}
		if err := validateContainer(m, lws, "workerTemplate", false); err != nil {
			return err
		}
	}
	return nil
}

// validateContainer checks one of the LWS pod templates against the
// model manifest: engine image and GPU count, plus the health contract
// when the template serves HTTP.
func validateContainer(m *manifest.Manifest, lws map[string]any, template string, probes bool) error {
	containers, _ := dig(lws, "spec", "leaderWorkerTemplate", template, "spec", "containers").([]any)
	if len(containers) == 0 {
		return fmt.Errorf("no containers in %s", template)
	}
	c, _ := containers[0].(map[string]any)

	image, _ := dig(c, "image").(string)
	switch m.Runtime {
	case manifest.RuntimeCustom:
		if image != m.Image {
			return fmt.Errorf("%s image %q, want manifest image %q", template, image, m.Image)
		}
	default:
		// Compare the repository component exactly, not by substring:
		// the registry prefix is the operator's to choose, but
		// llmops-runtime-sglang and llmops-runtime-sglang-k3 are
		// different engines and each contains the other's prefix.
		if got, want := imageName(image), m.EngineImage(); got != want {
			return fmt.Errorf("%s image %q does not match runtime %q: image name %q, want %q",
				template, image, m.Runtime, got, want)
		}
	}

	gpus := fmt.Sprint(dig(c, "resources", "limits", "nvidia.com/gpu"))
	if gpus != fmt.Sprint(m.GPU.Count) {
		return fmt.Errorf("%s nvidia.com/gpu %q, want %d", template, gpus, m.GPU.Count)
	}

	if !probes {
		return nil
	}
	if p, _ := dig(c, "readinessProbe", "httpGet", "path").(string); p != "/ready" {
		return fmt.Errorf("%s readinessProbe path %q, want /ready", template, p)
	}
	if p, _ := dig(c, "livenessProbe", "httpGet", "path").(string); p != "/healthz" {
		return fmt.Errorf("%s livenessProbe path %q, want /healthz", template, p)
	}
	return nil
}

// imageName reduces an image reference to its repository component:
// "nexus.example.com:5000/latere/llmops-runtime-sglang:v1" becomes
// "llmops-runtime-sglang". Registry, port, tag, and digest all drop.
func imageName(ref string) string {
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	return name
}

func splitDocs(data []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var docs []map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("empty yaml")
	}
	return docs, nil
}

// dig walks nested map[string]any by keys; returns nil when absent.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
