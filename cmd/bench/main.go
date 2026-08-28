// Command bench load-tests an OpenAI-compatible endpoint and writes a
// JSON report (specs/010-observability-bench.md).
//
//	bench --url http://host:8000 --model kimi-k2.7-code \
//	      [--concurrency 8] [--requests 32] [--max-tokens 128] \
//	      [--prompt "..."] [--out report.json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/latere-ai/llmops/internal/bench"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(errw)
	url := fs.String("url", "", "endpoint base URL")
	model := fs.String("model", "", "model id")
	concurrency := fs.Int("concurrency", 8, "concurrent requests")
	requests := fs.Int("requests", 32, "total requests")
	maxTokens := fs.Int("max-tokens", 128, "max tokens per request")
	prompt := fs.String("prompt", "Explain what a mutex is in two sentences.", "prompt")
	outPath := fs.String("out", "", "write JSON report to file (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rep, err := bench.Run(context.Background(), bench.Config{
		BaseURL:     *url,
		Model:       *model,
		Concurrency: *concurrency,
		Requests:    *requests,
		MaxTokens:   *maxTokens,
		Prompt:      *prompt,
	})
	if err != nil {
		fmt.Fprintln(errw, "bench:", err)
		return 1
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	if *outPath != "" {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			fmt.Fprintln(errw, "bench:", err)
			return 1
		}
		fmt.Fprintf(out, "report written to %s\n", *outPath)
		return 0
	}
	fmt.Fprintln(out, string(data))
	return 0
}
