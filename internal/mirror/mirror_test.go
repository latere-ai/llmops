package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// fakeHub serves the two HF API endpoints Mirror uses, backed by an
// in-memory file map. pageSize > 0 splits the tree response into cursor
// pages linked by a `Link: <…>; rel="next"` header, the way the real Hub
// paginates; pageSize <= 0 returns the whole tree in one response.
func fakeHub(t *testing.T, repo string, files map[string]string, lfs map[string]bool, pageSize int) *HFClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/"+repo, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"sha": testSHA})
	})
	mux.HandleFunc("/api/models/"+repo+"/revision/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"sha": testSHA})
	})
	mux.HandleFunc("/api/models/"+repo+"/tree/"+testSHA, func(w http.ResponseWriter, r *http.Request) {
		// Deterministic order: map iteration is random, and cursor
		// paging is only coherent over a stable sequence.
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		var tree []map[string]any
		for _, path := range paths {
			entry := map[string]any{"type": "file", "path": path, "size": len(files[path])}
			if lfs[path] {
				sum := sha256Hex(files[path])
				entry["lfs"] = map[string]any{"oid": "sha256:" + sum}
			}
			tree = append(tree, entry)
		}
		tree = append(tree, map[string]any{"type": "directory", "path": "subdir", "size": 0})

		if pageSize > 0 {
			offset := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				n, err := strconv.Atoi(c)
				if err != nil {
					http.Error(w, "bad cursor", http.StatusBadRequest)
					return
				}
				offset = n
			}
			if offset > len(tree) {
				offset = len(tree)
			}
			end := offset + pageSize
			if end >= len(tree) {
				end = len(tree)
			} else {
				next := "http://" + r.Host + r.URL.Path + "?recursive=true&cursor=" + strconv.Itoa(end)
				w.Header().Set("Link", "<"+next+`>; rel="next"`)
			}
			tree = tree[offset:end]
		}
		json.NewEncoder(w).Encode(tree)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &HFClient{Base: srv.URL, HTTP: srv.Client()}
}

func sha256Hex(content string) string {
	f, _ := os.CreateTemp("", "h")
	f.WriteString(content)
	f.Close()
	defer os.Remove(f.Name())
	sum, _ := FileSHA256(f.Name())
	return sum
}

// fakeDownloader materializes files into --local-dir, simulating
// `hf download`.
type fakeDownloader struct {
	files map[string]string
	calls int
	fail  bool
}

func (d *fakeDownloader) Run(_ context.Context, name string, args ...string) error {
	d.calls++
	if d.fail {
		return fmt.Errorf("simulated download failure")
	}
	if name != "hf" || args[0] != "download" {
		return fmt.Errorf("unexpected command %s %v", name, args)
	}
	dir := args[len(args)-1]
	for path, content := range d.files {
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

var testFiles = map[string]string{
	"config.json":                      `{"model_type":"test"}`,
	"model-00001-of-00002.safetensors": "weights-part-one",
	"model-00002-of-00002.safetensors": "weights-part-two",
}

func newMirror(t *testing.T, files map[string]string) (*Mirror, *fakeDownloader) {
	t.Helper()
	lfs := map[string]bool{
		"model-00001-of-00002.safetensors": true,
		"model-00002-of-00002.safetensors": true,
	}
	dl := &fakeDownloader{files: files}
	return &Mirror{HF: fakeHub(t, "acme/tiny", files, lfs, 2), Cmd: dl, Log: os.Stderr}, dl
}

func TestPullPushVerifyE2E(t *testing.T) {
	m, dl := newMirror(t, testFiles)
	dir := t.TempDir()

	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != testSHA || len(entries) != 3 {
		t.Fatalf("sha=%s entries=%d", sha, len(entries))
	}

	// Idempotent pull: second call verifies locally, no download.
	if _, _, err := m.Pull(context.Background(), "acme/tiny", "", dir); err != nil {
		t.Fatal(err)
	}
	if dl.calls != 1 {
		t.Fatalf("second pull re-downloaded (calls=%d)", dl.calls)
	}

	store := &LocalStore{Root: t.TempDir()}
	if err := m.Push("acme/tiny", sha, dir, entries, store); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(store); err != nil {
		t.Fatal(err)
	}

	sm, err := ReadManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	if sm.HFRepo != "acme/tiny" || sm.Revision != testSHA || sm.TotalBytes == 0 {
		t.Fatalf("manifest %+v", sm)
	}
}

func TestPushIdempotent(t *testing.T) {
	m, _ := newMirror(t, testFiles)
	dir := t.TempDir()
	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &countingStore{Store: &LocalStore{Root: t.TempDir()}}
	if err := m.Push("acme/tiny", sha, dir, entries, store); err != nil {
		t.Fatal(err)
	}
	first := store.puts
	if err := m.Push("acme/tiny", sha, dir, entries, store); err != nil {
		t.Fatal(err)
	}
	// Second push re-uploads only the manifest, not the files.
	if got := store.puts - first; got != 1 {
		t.Fatalf("second push uploaded %d objects, want 1 (manifest only)", got)
	}
}

// TestPushReplacesSameSizeDivergentObject pins content, not size, as the
// identity of a stored object. A stored object whose bytes diverge from
// the manifest at identical length must be re-uploaded: Push is the only
// repair path, so skipping it leaves the mirror permanently failing
// verify with no command that can fix it.
func TestPushReplacesSameSizeDivergentObject(t *testing.T) {
	m, _ := newMirror(t, testFiles)
	dir := t.TempDir()
	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}

	const target = "model-00001-of-00002.safetensors"
	root := t.TempDir()
	store := &LocalStore{Root: root}
	// Pre-seed divergent bytes of identical length ("weights-part-one").
	if err := os.WriteFile(filepath.Join(root, target), []byte("WEIGHTS-PART-XXX"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Push("acme/tiny", sha, dir, entries, store); err != nil {
		t.Fatal(err)
	}

	var want string
	for _, e := range entries {
		if e.Path == target {
			want = e.SHA256
		}
	}
	got, err := store.SHA256(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("stale object survived push: sha256 %s, want %s", got, want)
	}
	if err := m.Verify(store); err != nil {
		t.Fatalf("verify after push: %v", err)
	}
}

type countingStore struct {
	Store
	puts int
}

func (c *countingStore) Put(local, remote string) error {
	c.puts++
	return c.Store.Put(local, remote)
}

func TestVerifyDetectsCorruption(t *testing.T) {
	m, _ := newMirror(t, testFiles)
	dir := t.TempDir()
	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := &LocalStore{Root: root}
	if err := m.Push("acme/tiny", sha, dir, entries, store); err != nil {
		t.Fatal(err)
	}

	// Same-size corruption: only the hash can catch it.
	target := filepath.Join(root, "model-00001-of-00002.safetensors")
	if err := os.WriteFile(target, []byte("weights-part-XXX"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.Verify(store)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("corruption not detected: %v", err)
	}

	// Truncation: size check catches it.
	if err := os.WriteFile(target, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.Verify(store)
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("truncation not detected: %v", err)
	}
}

func TestPolicyRejectsPickle(t *testing.T) {
	files := map[string]string{"pytorch_model.bin": "pickle-bytes"}
	m, _ := newMirror(t, files)
	_, _, err := m.Pull(context.Background(), "acme/tiny", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "safetensors-only") {
		t.Fatalf("pickle weights not rejected: %v", err)
	}
}

func TestPullDownloadFailure(t *testing.T) {
	m, dl := newMirror(t, testFiles)
	dl.fail = true
	if _, _, err := m.Pull(context.Background(), "acme/tiny", "", t.TempDir()); err == nil {
		t.Fatal("download failure must propagate")
	}
}

func TestPullCorruptedDownload(t *testing.T) {
	files := map[string]string{"model.safetensors": "good-weights"}
	lfs := map[string]bool{"model.safetensors": true}
	hub := fakeHub(t, "acme/tiny", files, lfs, 2)
	// Downloader writes different content than the tree advertises.
	dl := &fakeDownloader{files: map[string]string{"model.safetensors": "bad-weights!"}}
	m := &Mirror{HF: hub, Cmd: dl}
	_, _, err := m.Pull(context.Background(), "acme/tiny", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("hash mismatch not detected: %v", err)
	}
}

func TestReadManifestMissing(t *testing.T) {
	store := &LocalStore{Root: t.TempDir()}
	if _, err := ReadManifest(store); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("missing manifest must report incomplete mirror: %v", err)
	}
	// Corrupt manifest JSON.
	os.WriteFile(filepath.Join(store.Root, ManifestName), []byte("{"), 0o644)
	if _, err := ReadManifest(store); err == nil {
		t.Fatal("corrupt manifest must error")
	}
}

func TestResolveErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "empty") {
			json.NewEncoder(w).Encode(map[string]string{})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &HFClient{Base: srv.URL, HTTP: srv.Client()}
	if _, err := c.Resolve("acme/missing", ""); err == nil {
		t.Fatal("404 must error")
	}
	if _, err := c.Resolve("acme/empty", ""); err == nil {
		t.Fatal("missing sha must error")
	}
	if _, err := c.Tree("acme/missing", testSHA); err == nil {
		t.Fatal("tree 404 must error")
	}
}

func TestTreeEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	c := &HFClient{Base: srv.URL, HTTP: srv.Client()}
	if _, err := c.Tree("acme/tiny", testSHA); err == nil {
		t.Fatal("empty tree must error")
	}
}

// TestTreeFollowsPagination pins the Hub's cursor paging: a tree larger
// than one page must be assembled from every page, not just the first.
// A truncated tree is silently self-consistent downstream (manifest,
// verify, policy all see the same short list), so it can only be caught
// here.
func TestTreeFollowsPagination(t *testing.T) {
	files := map[string]string{
		"a.safetensors": "aaaa",
		"b.safetensors": "bbbb",
		"c.safetensors": "cccc",
		"d.safetensors": "dddd",
	}
	c := fakeHub(t, "acme/tiny", files, nil, 2)
	got, err := c.Tree("acme/tiny", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(files) {
		t.Fatalf("Tree returned %d files, want %d", len(got), len(files))
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.Path] = true
	}
	for path := range files {
		if !seen[path] {
			t.Errorf("Tree dropped %s", path)
		}
	}
}

// TestTreeRejectsCrossHostPagination pins the trust boundary: the next
// URL is server-supplied, so following it off the configured Base host
// would let a compromised or spoofed response redirect metadata fetches.
func TestTreeRejectsCrossHostPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<http://evil.example/api/models/acme/tiny/tree/x?cursor=2>; rel="next"`)
		json.NewEncoder(w).Encode([]map[string]any{
			{"type": "file", "path": "a.safetensors", "size": 4},
		})
	}))
	defer srv.Close()
	c := &HFClient{Base: srv.URL, HTTP: srv.Client()}
	if _, err := c.Tree("acme/tiny", testSHA); err == nil ||
		!strings.Contains(err.Error(), "host") {
		t.Fatalf("cross-host next link not rejected: %v", err)
	}
}

// TestTreeCapsPageCount pins the loop bound: a server that always
// advertises another page must not hang the client forever.
func TestTreeCapsPageCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "<http://"+r.Host+"/api/models/acme/tiny/tree/x?cursor=1>; rel=\"next\"")
		json.NewEncoder(w).Encode([]map[string]any{
			{"type": "file", "path": "a.safetensors", "size": 4},
		})
	}))
	defer srv.Close()
	c := &HFClient{Base: srv.URL, HTTP: srv.Client()}
	if _, err := c.Tree("acme/tiny", testSHA); err == nil ||
		!strings.Contains(err.Error(), "too many pages") {
		t.Fatalf("unbounded pagination not capped: %v", err)
	}
}

func TestNewHFClientDefaults(t *testing.T) {
	c := NewHFClient()
	if c.Base != "https://huggingface.co" || c.HTTP == nil {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOpenStore(t *testing.T) {
	if _, ok := OpenStore("s3://bucket/x/").(*S5cmdStore); !ok {
		t.Fatal("s3:// must open S5cmdStore")
	}
	ls, ok := OpenStore("file:///tmp/x").(*LocalStore)
	if !ok || ls.Root != "/tmp/x" {
		t.Fatalf("file:// must open LocalStore at path, got %+v", ls)
	}
	if _, ok := OpenStore("/tmp/y").(*LocalStore); !ok {
		t.Fatal("plain path must open LocalStore")
	}
}

func TestLocalStoreEdges(t *testing.T) {
	s := &LocalStore{Root: filepath.Join(t.TempDir(), "missing")}
	files, err := s.List()
	if err != nil || files != nil {
		t.Fatalf("List on missing root: %v %v", files, err)
	}
	if size, err := s.Size("nope"); err != nil || size != -1 {
		t.Fatalf("Size on missing file: %d %v", size, err)
	}
	if err := s.Get("nope", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("Get missing must error")
	}
	if _, err := s.SHA256("nope"); err == nil {
		t.Fatal("SHA256 missing must error")
	}
	if err := s.Put(filepath.Join(t.TempDir(), "absent"), "x"); err == nil {
		t.Fatal("Put missing local must error")
	}
}

// TestS5cmdStore exercises the S3 path against a fake s5cmd on PATH that
// emulates cp/ls with the local filesystem.
func TestS5cmdStore(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	script := `#!/bin/sh
ROOT="$S5CMD_FAKE_ROOT"
cmd="$1"; shift
strip() { echo "$1" | sed 's|^s3://bucket/|'"$ROOT"'/|'; }
case "$cmd" in
cp)
  src=$(strip "$1"); dst=$(strip "$2")
  mkdir -p "$(dirname "$dst")"
  cp "$src" "$dst" 2>/dev/null || exit 1
  ;;
ls)
  pat=$(strip "$1")
  case "$pat" in
  *'*') found=0
     for f in $(find "${pat%\*}" -type f 2>/dev/null); do echo "2026/07/18 00:00:00 $(wc -c < "$f" | tr -d ' ') s3://bucket/${f#"$ROOT"/}"; found=1; done
     [ "$found" = 1 ] || exit 1 ;;
  *) if [ ! -f "$pat" ]; then
       echo "ERROR \"ls $1\": no object found" >&2
       exit 1
     fi
     echo "2026/07/18 00:00:00 $(wc -c < "$pat" | tr -d ' ') s3://bucket/${pat#"$ROOT"/}" ;;
  esac
  ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "s5cmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("S5CMD_FAKE_ROOT", root)

	s := OpenStore("s3://bucket/").(*S5cmdStore)
	local := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(local, []byte("hello"), 0o644)

	if err := s.Put(local, "dir/f.txt"); err != nil {
		t.Fatal(err)
	}
	if size, err := s.Size("dir/f.txt"); err != nil || size != 5 {
		t.Fatalf("Size = %d, %v", size, err)
	}
	if size, err := s.Size("absent"); err != nil || size != -1 {
		t.Fatalf("Size absent = %d, %v", size, err)
	}
	sum, err := s.SHA256("dir/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := FileSHA256(local)
	if sum != want {
		t.Fatalf("SHA256 = %s, want %s", sum, want)
	}
	files, err := s.List()
	if err != nil || len(files) != 1 || files[0] != "dir/f.txt" {
		t.Fatalf("List = %v, %v", files, err)
	}
	out := filepath.Join(t.TempDir(), "out.txt")
	if err := s.Get("dir/f.txt", out); err != nil {
		t.Fatal(err)
	}
	if err := s.Get("absent", out); err == nil {
		t.Fatal("Get absent must error")
	}
}

func TestS5cmdStoreSizePropagatesOperationalError(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
echo 'ERROR "ls s3://bucket/x": InvalidAccessKeyId: invalid credentials' >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "s5cmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	size, err := (&S5cmdStore{Prefix: "s3://bucket/"}).Size("x")
	if err == nil {
		t.Fatalf("Size returned (%d, nil) for an operational s5cmd failure", size)
	}
	if !strings.Contains(err.Error(), "InvalidAccessKeyId") {
		t.Fatalf("Size error lost s5cmd cause: %v", err)
	}
}

func TestExecCommander(t *testing.T) {
	c := &ExecCommander{}
	if err := c.Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(context.Background(), "false"); err == nil {
		t.Fatal("false must fail")
	}
}

func TestGetJSONDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	c := &HFClient{Base: srv.URL, HTTP: srv.Client()}
	if _, err := c.Resolve("acme/tiny", ""); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestResolveWithRevision(t *testing.T) {
	c := fakeHub(t, "acme/tiny", testFiles, nil, 0)
	sha, err := c.Resolve("acme/tiny", "main")
	if err != nil || sha != testSHA {
		t.Fatalf("Resolve with revision = %s, %v", sha, err)
	}
}

func TestPullResolveAndTreeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"sha": testSHA})
	}))
	defer srv.Close()
	m := &Mirror{HF: &HFClient{Base: srv.URL, HTTP: srv.Client()}, Cmd: &fakeDownloader{}}
	if _, _, err := m.Pull(context.Background(), "acme/tiny", "", t.TempDir()); err == nil {
		t.Fatal("tree error must propagate")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv2.Close()
	m2 := &Mirror{HF: &HFClient{Base: srv2.URL, HTTP: srv2.Client()}, Cmd: &fakeDownloader{}}
	if _, _, err := m2.Pull(context.Background(), "acme/tiny", "", t.TempDir()); err == nil {
		t.Fatal("resolve error must propagate")
	}
}

func TestPullIncompleteDownload(t *testing.T) {
	// Downloader omits one advertised file entirely, and writes another
	// with the wrong size.
	files := map[string]string{"a.safetensors": "aaaa", "b.safetensors": "bbbb"}
	hub := fakeHub(t, "acme/tiny", files, nil, 2)
	dl := &fakeDownloader{files: map[string]string{"a.safetensors": "aa-too-long"}}
	m := &Mirror{HF: hub, Cmd: dl}
	_, _, err := m.Pull(context.Background(), "acme/tiny", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "post-download") {
		t.Fatalf("incomplete download not detected: %v", err)
	}
}

// failStore errors on selected operations to reach Push/Verify branches.
type failStore struct {
	Store
	failSize bool
	failSHA  bool
	failPut  bool
}

func (f *failStore) Size(remote string) (int64, error) {
	if f.failSize {
		return -1, fmt.Errorf("size boom")
	}
	return f.Store.Size(remote)
}
func (f *failStore) SHA256(remote string) (string, error) {
	if f.failSHA {
		return "", fmt.Errorf("sha boom")
	}
	return f.Store.SHA256(remote)
}
func (f *failStore) Put(local, remote string) error {
	if f.failPut {
		return fmt.Errorf("put boom")
	}
	return f.Store.Put(local, remote)
}

func TestPushStoreErrors(t *testing.T) {
	m, _ := newMirror(t, testFiles)
	dir := t.TempDir()
	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	base := &LocalStore{Root: t.TempDir()}
	if err := m.Push("acme/tiny", sha, dir, entries, &failStore{Store: base, failSize: true}); err == nil {
		t.Fatal("Size error must propagate")
	}
	if err := m.Push("acme/tiny", sha, dir, entries, &failStore{Store: base, failPut: true}); err == nil {
		t.Fatal("Put error must propagate")
	}
}

func TestVerifyStoreErrors(t *testing.T) {
	m, _ := newMirror(t, testFiles)
	dir := t.TempDir()
	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	base := &LocalStore{Root: t.TempDir()}
	if err := m.Push("acme/tiny", sha, dir, entries, base); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(&failStore{Store: base, failSize: true}); err == nil {
		t.Fatal("Size error must propagate")
	}
	if err := m.Verify(&failStore{Store: base, failSHA: true}); err == nil {
		t.Fatal("SHA256 error must propagate")
	}
}

func TestLocalStorePutBadRoot(t *testing.T) {
	// Root is a file, so MkdirAll under it fails.
	rootFile := filepath.Join(t.TempDir(), "rootfile")
	os.WriteFile(rootFile, []byte("x"), 0o644)
	s := &LocalStore{Root: rootFile}
	local := filepath.Join(t.TempDir(), "f")
	os.WriteFile(local, []byte("y"), 0o644)
	if err := s.Put(local, "sub/f"); err == nil {
		t.Fatal("Put under file root must error")
	}
}

func TestLocalStoreListUnreadable(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o644)
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Skip("cannot chmod")
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })
	if _, err := (&LocalStore{Root: root}).List(); err == nil {
		t.Fatal("unreadable subdir must error")
	}
}

func TestS5cmdStoreErrors(t *testing.T) {
	bin := t.TempDir()
	// A fake s5cmd that emits garbage for ls and fails cp.
	script := "#!/bin/sh\ncase \"$1\" in\nls) echo garbage ;;\n*) exit 1 ;;\nesac\n"
	os.WriteFile(filepath.Join(bin, "s5cmd"), []byte(script), 0o755)
	t.Setenv("PATH", bin)
	s := &S5cmdStore{Prefix: "s3://bucket/"}
	if _, err := s.Size("x"); err == nil {
		t.Fatal("garbage ls output must error")
	}
	if err := s.Put("/nonexistent", "x"); err == nil {
		t.Fatal("cp failure must error")
	}
	if _, err := s.SHA256("x"); err == nil {
		t.Fatal("SHA256 with failing get must error")
	}
}

func TestGetJSONConnectionError(t *testing.T) {
	c := &HFClient{Base: "http://127.0.0.1:1", HTTP: http.DefaultClient}
	if _, err := c.Resolve("acme/tiny", ""); err == nil {
		t.Fatal("connection error must propagate")
	}
}

func TestLocalStoreListSkipsTempFiles(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".put-123"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(root, "real"), []byte("x"), 0o644)
	files, err := (&LocalStore{Root: root}).List()
	if err != nil || len(files) != 1 || files[0] != "real" {
		t.Fatalf("List = %v, %v (want [real])", files, err)
	}
}

func TestLocalStoreGetSizeParentIsFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o644)
	s := &LocalStore{Root: root}
	// Destination parent is an existing file → MkdirAll fails.
	local := filepath.Join(t.TempDir(), "f")
	os.WriteFile(local, []byte("y"), 0o644)
	if err := s.Get("blocker", filepath.Join(local, "under-file")); err == nil {
		t.Fatal("Get with file-parent dest must error")
	}
	// Stat through a file component → ENOTDIR, not IsNotExist.
	if _, err := s.Size("blocker/child"); err == nil {
		t.Fatal("Size through file component must error")
	}
}

func TestLocalStorePutUnwritableDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Skip("cannot chmod")
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })
	local := filepath.Join(t.TempDir(), "f")
	os.WriteFile(local, []byte("y"), 0o644)
	if err := (&LocalStore{Root: root}).Put(local, "sub/f"); err == nil {
		t.Fatal("Put into unwritable dir must error")
	}
}

type manifestFailStore struct{ Store }

func (m *manifestFailStore) Put(local, remote string) error {
	if remote == ManifestName {
		return fmt.Errorf("manifest put boom")
	}
	return m.Store.Put(local, remote)
}

func TestPushManifestWriteFailure(t *testing.T) {
	m, _ := newMirror(t, testFiles)
	dir := t.TempDir()
	sha, entries, err := m.Pull(context.Background(), "acme/tiny", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &manifestFailStore{Store: &LocalStore{Root: t.TempDir()}}
	if err := m.Push("acme/tiny", sha, dir, entries, store); err == nil {
		t.Fatal("manifest put failure must propagate")
	}
}
