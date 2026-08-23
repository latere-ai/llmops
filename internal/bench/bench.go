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
	Config     Config  `json:"config"`
	Requests   int     `json:"requests"`
	Errors     int     `json:"errors"`
	Chunks     int     `json:"chunks"`
	DurationS  float64 `json:"duration_s"`
	TTFTp50Ms  float64 `json:"ttft_p50_ms"`
	TTFTp95Ms  float64 `json:"ttft_p95_ms"`
	ChunksPerS float64 `json:"chunks_per_s"`
}

type sample struct {
	ttft   time.Duration
	chunks int
	err    error
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
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSecs * float64(time.Second))}

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
		ttfts = append(ttfts, s.ttft)
	}
	if len(ttfts) > 0 {
		slices.Sort(ttfts)
		rep.TTFTp50Ms = float64(percentile(ttfts, 50)) / float64(time.Millisecond)
		rep.TTFTp95Ms = float64(percentile(ttfts, 95)) / float64(time.Millisecond)
	}
	if elapsed > 0 {
		rep.ChunksPerS = float64(rep.Chunks) / elapsed.Seconds()
	}
	return rep, nil
}

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
	body, _ := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"stream":     true,
		"max_tokens": cfg.MaxTokens,
		"messages":   []map[string]string{{"role": "user", "content": cfg.Prompt}},
	})
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{err: fmt.Errorf("status %s", resp.Status)}
	}

	var s sample
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
	}
	if err := scanner.Err(); err != nil {
		return sample{err: err}
	}
	if s.chunks == 0 {
		return sample{err: fmt.Errorf("no stream chunks received")}
	}
	return s
}
