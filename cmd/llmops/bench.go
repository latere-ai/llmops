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

// runBench load-tests a live endpoint and writes a JSON report
// (specs/010-observability-bench.md).
func runBench(rest []string, out, errw io.Writer) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(errw)
	url := fs.String("url", "", "endpoint base URL")
	model := fs.String("model", "", "model id")
	concurrency := fs.Int("concurrency", 8, "concurrent requests")
	requests := fs.Int("requests", 32, "total requests")
	maxTokens := fs.Int("max-tokens", 128, "max tokens per request")
	prompt := fs.String("prompt", "Explain what a mutex is in two sentences.", "prompt")
	outPath := fs.String("out", "", "write JSON report to file (default stdout)")
	if err := fs.Parse(rest); err != nil {
		return usagef("usage: llmops bench --url <base> --model <id>")
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
		return err
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	if *outPath != "" {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "report written to %s\n", *outPath)
		return nil
	}
	fmt.Fprintln(out, string(data))
	return nil
}
