package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
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

// get issues one Hub request under the caller's context, so a cancelled
// `llmops weights` stops the metadata fetch instead of running it out.
func (c *HFClient) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.HTTP.Do(req)
}

func (c *HFClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.get(ctx, c.Base+path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// maxTreePages bounds Link-following. The Hub pages trees at 1000
// entries, so this admits a million-file repo while stopping a cyclic or
// unbounded cursor chain from looping forever.
const maxTreePages = 1000

// getJSONArrayPaged fetches a cursor-paginated JSON array endpoint and
// concatenates every page. The Hub signals continuation with a
// `Link: <url>; rel="next"` header (huggingface_hub paginates
// list_repo_tree the same way); without following it a caller silently
// sees only the first page.
func getJSONArrayPaged[T any](ctx context.Context, c *HFClient, path string) ([]T, error) {
	base, err := url.Parse(c.Base)
	if err != nil {
		return nil, fmt.Errorf("parse base %q: %w", c.Base, err)
	}
	var all []T
	next := c.Base + path
	for page := 0; next != ""; page++ {
		if page >= maxTreePages {
			return nil, fmt.Errorf("GET %s: too many pages (>%d)", path, maxTreePages)
		}
		items, link, err := getJSONArrayPage[T](ctx, c, next, path)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		next, err = nextPageURL(link, base)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
	}
	return all, nil
}

// getJSONArrayPage fetches one absolute page URL, returning its decoded
// items and its raw Link header. path is only used for error messages.
func getJSONArrayPage[T any](ctx context.Context, c *HFClient, rawURL, path string) ([]T, string, error) {
	resp, err := c.get(ctx, rawURL)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	var items []T
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, "", err
	}
	return items, resp.Header.Get("Link"), nil
}

// nextPageURL extracts the rel="next" target from a Link header. The URL
// is server-supplied, so it is accepted only when it points at the same
// host as Base; anything else would let a response redirect metadata
// fetches off the configured Hub.
func nextPageURL(link string, base *url.URL) (string, error) {
	for part := range strings.SplitSeq(link, ",") {
		lt := strings.Index(part, "<")
		gt := strings.Index(part, ">")
		if lt < 0 || gt < lt {
			continue
		}
		params := strings.ToLower(part[gt:])
		if !strings.Contains(params, `rel="next"`) && !strings.Contains(params, "rel=next") {
			continue
		}
		raw := strings.TrimSpace(part[lt+1 : gt])
		u, err := base.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("bad next link %q: %w", raw, err)
		}
		if u.Host != base.Host || u.Scheme != base.Scheme {
			return "", fmt.Errorf("next link host %q does not match base host %q",
				u.Scheme+"://"+u.Host, base.Scheme+"://"+base.Host)
		}
		return u.String(), nil
	}
	return "", nil
}

// Resolve returns the commit SHA for a repo revision (branch, tag, or
// SHA; empty means the default branch).
func (c *HFClient) Resolve(ctx context.Context, repo, revision string) (string, error) {
	p := "/api/models/" + repo
	if revision != "" {
		p += "/revision/" + url.PathEscape(revision)
	}
	var info struct {
		SHA string `json:"sha"`
	}
	if err := c.getJSON(ctx, p, &info); err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", repo, revision, err)
	}
	if info.SHA == "" {
		return "", fmt.Errorf("resolve %s@%s: no sha in response", repo, revision)
	}
	return info.SHA, nil
}

// treeEntry is one raw entry of the Hub tree endpoint.
type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		OID string `json:"oid"`
	} `json:"lfs"`
}

// Tree lists all files at a revision, recursively. The endpoint is
// cursor-paginated (1000 entries per page), so every page is followed:
// a partial tree would propagate into the manifest and make verify pass
// on an incomplete mirror.
func (c *HFClient) Tree(ctx context.Context, repo, sha string) ([]TreeFile, error) {
	p := "/api/models/" + repo + "/tree/" + sha + "?recursive=true"
	raw, err := getJSONArrayPaged[treeEntry](ctx, c, p)
	if err != nil {
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

// allowedMirrorExts makes the safetensors-only policy fail closed. New
// weight formats cannot enter the frozen registry merely because their
// extension was absent from a deny-list. Additions here must be formats
// that cannot carry model weights.
var allowedMirrorExts = map[string]bool{
	// Model weights.
	".safetensors": true,

	// Config, documentation, metadata, and source code.
	".bash": true, ".c": true, ".cc": true, ".cfg": true,
	".conf": true, ".cpp": true, ".csv": true, ".cu": true,
	".cuh": true, ".cxx": true, ".dockerignore": true,
	".editorconfig": true, ".gitattributes": true, ".gitignore": true,
	".gitmodules": true, ".h": true, ".hh": true, ".hpp": true,
	".ini": true, ".jinja": true, ".jinja2": true, ".js": true,
	".json": true, ".jsonl": true, ".lock": true, ".md": true,
	".pdf": true, ".py": true, ".pyi": true, ".rst": true,
	".sh": true, ".svg": true, ".toml": true, ".ts": true,
	".tsv": true, ".txt": true, ".xml": true, ".yaml": true,
	".yml": true,

	// Tokenizer assets and model-card media.
	".gif": true, ".jpeg": true, ".jpg": true, ".merges": true,
	".model": true, ".mp4": true, ".png": true, ".spm": true,
	".tiktoken": true, ".vocab": true, ".webp": true,
}

var allowedExtensionlessFiles = map[string]bool{
	"authors": true, "citation": true, "code_of_conduct": true,
	"contributors": true, "dockerfile": true, "license": true,
	"makefile": true, "notice": true, "readme": true,
	"security": true, "use_policy": true,
}

// CheckPolicy permits safetensors weights and known non-weight companions.
// Unknown file types require explicit review before an immutable mirror is
// created; this is intentionally a fail-closed allowlist (specs/002 AC3).
func CheckPolicy(files []TreeFile) error {
	for _, f := range files {
		name := strings.ToLower(path.Base(f.Path))
		if allowedMirrorExts[strings.ToLower(path.Ext(name))] || allowedExtensionlessFiles[name] {
			continue
		}
		return fmt.Errorf("policy: %s violates safetensors-only weights policy: file type is not an approved non-weight companion", f.Path)
	}
	return nil
}
