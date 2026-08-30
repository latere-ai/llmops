// Package bench drives an OpenAI-compatible endpoint and reports
// latency/throughput (specs/010-observability-bench.md). It measures the
// serving path only, not answer quality.
package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"
)

// Config is one bench run.
type Config struct {
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	Concurrency int     `json:"concurrency"`
	Requests    int     `json:"requests"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	TimeoutSecs float64 `json:"timeout_secs,omitempty"`
}

// Report is the machine-readable result (stable format; golden-tested).
type Report struct {
	Config    Config  `json:"config"`
	Requests  int     `json:"requests"`
	Errors    int     `json:"errors"`
	Chunks    int     `json:"chunks"`
	Tokens    int     `json:"tokens"`
	DurationS float64 `json:"duration_s"`
	TTFTp50Ms float64 `json:"ttft_p50_ms"`
	TTFTp95Ms float64 `json:"ttft_p95_ms"`

	// ChunksPerS counts server-sent events, not tokens.
	//
	// Keep the distinction: one SSE event carries one token only when
	// the engine emits them one at a time. Speculative decoding commits
	// several tokens per verify step and can pack them into one event,
	// so this figure understates real throughput — a published
	// measurement of this model was wrong for exactly that reason.
	// TokensPerS is the number to quote.
	ChunksPerS float64 `json:"chunks_per_s"`
	// TokensPerS is generated tokens per second, from the engine's own
	// usage accounting. Zero when the endpoint reports no usage.
	TokensPerS float64 `json:"tokens_per_s"`

	// Speculator is the draft-model configuration the endpoint reported
	// serving with (specs/027). A throughput figure for a model that
	// offers several means little without it: the same endpoint answers
	// at 51 tok/s on code with one draft head and 18 with another.
	Speculator string `json:"speculator,omitempty"`
}

type sample struct {
	ttft       time.Duration
	chunks     int
	tokens     int
	speculator string
	err        error
}

// Run executes the configured load and aggregates a report.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, fmt.Errorf("base_url and model are required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Requests <= 0 {
		cfg.Requests = cfg.Concurrency
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 128
	}
	if cfg.TimeoutSecs <= 0 {
		cfg.TimeoutSecs = 120
	}
	// The benchmark measures the endpoint it calls, so its requests belong
	// in the same trace as the serving they provoke: a slow sample is then
	// answerable with the server and engine spans underneath it rather
	// than a number with no explanation.
	client := &http.Client{Transport: otel.Transport(nil), Timeout: time.Duration(cfg.TimeoutSecs * float64(time.Second))}

	jobs := make(chan int)
	samples := make([]sample, cfg.Requests)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Go(func() {
			for i := range jobs {
				samples[i] = oneRequest(ctx, client, cfg)
			}
		})
	}
	for i := 0; i < cfg.Requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	rep := &Report{Config: cfg, Requests: cfg.Requests, DurationS: elapsed.Seconds()}
	var ttfts []time.Duration
	for _, s := range samples {
		if s.err != nil {
			rep.Errors++
			continue
		}
		rep.Chunks += s.chunks
		rep.Tokens += s.tokens
		ttfts = append(ttfts, s.ttft)
		// Every request hits the same endpoint, so the first answer is
		// the answer. A disagreement means the endpoint restarted with a
		// different head mid-run, which invalidates the aggregate.
		switch {
		case rep.Speculator == "":
			rep.Speculator = s.speculator
		case s.speculator != "" && s.speculator != rep.Speculator:
			return nil, fmt.Errorf("endpoint changed speculator mid-run (%s then %s); rerun against a stable endpoint",
				rep.Speculator, s.speculator)
		}
	}
	if len(ttfts) > 0 {
		slices.Sort(ttfts)
		rep.TTFTp50Ms = float64(percentile(ttfts, 50)) / float64(time.Millisecond)
		rep.TTFTp95Ms = float64(percentile(ttfts, 95)) / float64(time.Millisecond)
	}
	if elapsed > 0 {
		rep.ChunksPerS = float64(rep.Chunks) / elapsed.Seconds()
		rep.TokensPerS = float64(rep.Tokens) / elapsed.Seconds()
	}
	return rep, nil
}

// SpeculatorHeader is the response header naming the active draft-model
// configuration. It mirrors the constant the shim sets; this package is
// deliberately free of llmops imports so it can be pointed at any
// OpenAI-compatible endpoint, ours or not.
const SpeculatorHeader = "X-LLMOps-Speculator"

// percentile is the nearest-rank method: idx = ceil(p/100 * n) - 1.
func percentile(sorted []time.Duration, p int) time.Duration {
	idx := (p*len(sorted) + 99) / 100
	if idx > 0 {
		idx--
	}
	return sorted[idx]
}

// oneRequest streams one chat completion and records TTFT + chunk count.
func oneRequest(ctx context.Context, client *http.Client, cfg Config) sample {
	body, err := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"stream":     true,
		"max_tokens": cfg.MaxTokens,
		"messages":   []map[string]string{{"role": "user", "content": cfg.Prompt}},
		// Ask for the usage record in the final chunk. It is the only
		// count of tokens the engine actually generated; everything else
		// available here counts transport events.
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return sample{err: fmt.Errorf("encode request: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return sample{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return sample{err: fmt.Errorf("status %s", resp.Status)}
	}

	var s sample
	s.speculator = resp.Header.Get(SpeculatorHeader)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		if s.chunks == 0 {
			s.ttft = time.Since(start)
		}
		s.chunks++

		// The usage chunk arrives last and carries no choices. Read the
		// count rather than inferring it from the number of events.
		var frame struct {
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &frame) == nil && frame.Usage != nil {
			s.tokens = frame.Usage.CompletionTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return sample{err: err}
	}
	if s.chunks == 0 {
		return sample{err: fmt.Errorf("no stream chunks received")}
	}
	return s
}
