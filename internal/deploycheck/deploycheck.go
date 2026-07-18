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
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/latere-ai/open-llms/internal/manifest"
)

// DNSName converts a model name to its k8s resource name.
func DNSName(model string) string { return strings.ReplaceAll(model, ".", "-") }

// Validate checks every model manifest has a deploy/<name>/lws.yaml
// consistent with it.
func Validate(modelsDir, deployDir string) error {
	models, err := manifest.LoadDir(modelsDir)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no model manifests in %s", modelsDir)
	}
	for _, m := range models {
		path := filepath.Join(deployDir, m.Name, "lws.yaml")
		if err := validateOne(m, path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
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

	containers, _ := dig(lws, "spec", "leaderWorkerTemplate", "leaderTemplate", "spec", "containers").([]any)
	if len(containers) == 0 {
		return fmt.Errorf("no containers in leaderTemplate")
	}
	c, _ := containers[0].(map[string]any)

	image, _ := dig(c, "image").(string)
	switch m.Runtime {
	case manifest.RuntimeCustom:
		if image != m.Image {
			return fmt.Errorf("image %q, want manifest image %q", image, m.Image)
		}
	default:
		if !strings.Contains(image, "open-llms-runtime-"+m.Runtime) {
			return fmt.Errorf("image %q does not match runtime %q", image, m.Runtime)
		}
	}

	gpus := fmt.Sprint(dig(c, "resources", "limits", "nvidia.com/gpu"))
	if gpus != fmt.Sprint(m.GPU.Count) {
		return fmt.Errorf("nvidia.com/gpu %q, want %d", gpus, m.GPU.Count)
	}

	if p, _ := dig(c, "readinessProbe", "httpGet", "path").(string); p != "/ready" {
		return fmt.Errorf("readinessProbe path %q, want /ready", p)
	}
	if p, _ := dig(c, "livenessProbe", "httpGet", "path").(string); p != "/healthz" {
		return fmt.Errorf("livenessProbe path %q, want /healthz", p)
	}
	return nil
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
