package mirror

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TreeFile is one file in an HF repo revision. SHA256 is set for LFS
// files (HF LFS OIDs are SHA256 — specs/002); small non-LFS files carry
// only a size and are hashed locally at pull time.
type TreeFile struct {
	Path   string
	Size   int64
	SHA256 string
}

// HFClient talks to the Hugging Face Hub API. Base is injectable for
// tests (default https://huggingface.co).
type HFClient struct {
	Base string
	HTTP *http.Client
}

func NewHFClient() *HFClient {
	return &HFClient{Base: "https://huggingface.co", HTTP: http.DefaultClient}
}

func (c *HFClient) getJSON(path string, out any) error {
	resp, err := c.HTTP.Get(c.Base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Resolve returns the commit SHA for a repo revision (branch, tag, or
// SHA; empty means the default branch).
func (c *HFClient) Resolve(repo, revision string) (string, error) {
	p := "/api/models/" + repo
	if revision != "" {
		p += "/revision/" + url.PathEscape(revision)
	}
	var info struct {
		SHA string `json:"sha"`
	}
	if err := c.getJSON(p, &info); err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", repo, revision, err)
	}
	if info.SHA == "" {
		return "", fmt.Errorf("resolve %s@%s: no sha in response", repo, revision)
	}
	return info.SHA, nil
}

// Tree lists all files at a revision, recursively.
func (c *HFClient) Tree(repo, sha string) ([]TreeFile, error) {
	var raw []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			OID string `json:"oid"`
		} `json:"lfs"`
	}
	p := "/api/models/" + repo + "/tree/" + sha + "?recursive=true"
	if err := c.getJSON(p, &raw); err != nil {
		return nil, fmt.Errorf("tree %s@%s: %w", repo, sha, err)
	}
	var files []TreeFile
	for _, f := range raw {
		if f.Type != "file" {
			continue
		}
		tf := TreeFile{Path: f.Path, Size: f.Size}
		if f.LFS != nil {
			tf.SHA256 = strings.TrimPrefix(f.LFS.OID, "sha256:")
		}
		files = append(files, tf)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("tree %s@%s: empty", repo, sha)
	}
	return files, nil
}

// forbiddenWeightExts is the safetensors-only policy (specs/002 AC3):
// serialized-python and other mutable weight formats are rejected.
var forbiddenWeightExts = map[string]bool{
	".bin": true, ".pt": true, ".pth": true, ".pkl": true,
	".pickle": true, ".ckpt": true, ".h5": true,
}

// CheckPolicy rejects trees containing forbidden weight formats.
func CheckPolicy(files []TreeFile) error {
	for _, f := range files {
		for ext := range forbiddenWeightExts {
			if strings.HasSuffix(f.Path, ext) {
				return fmt.Errorf("policy: %s violates safetensors-only weights policy", f.Path)
			}
		}
	}
	return nil
}
