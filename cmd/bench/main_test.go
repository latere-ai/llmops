package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBenchToStdout(t *testing.T) {
	srv := fakeEndpoint(t)
	var out, errw strings.Builder
	code := run([]string{"--url", srv.URL, "--model", "m", "--requests", "2", "--concurrency", "2"}, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if rep["requests"].(float64) != 2 {
		t.Fatalf("report %v", rep)
	}
}

func TestBenchToFile(t *testing.T) {
	srv := fakeEndpoint(t)
	path := filepath.Join(t.TempDir(), "report.json")
	var out, errw strings.Builder
	if code := run([]string{"--url", srv.URL, "--model", "m", "--requests", "1", "--out", path}, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("report file: %v, %s", err, data)
	}
}

func TestBenchErrors(t *testing.T) {
	var out, errw strings.Builder
	if code := run([]string{"--model", "m"}, &out, &errw); code == 0 {
		t.Fatal("missing url must fail")
	}
	if code := run([]string{"--badflag"}, &out, &errw); code != 2 {
		t.Fatal("bad flag must exit 2")
	}
	srv := fakeEndpoint(t)
	if code := run([]string{"--url", srv.URL, "--model", "m", "--out", "/nonexistent-dir/x.json"}, &out, &errw); code == 0 {
		t.Fatal("unwritable out must fail")
	}
}
