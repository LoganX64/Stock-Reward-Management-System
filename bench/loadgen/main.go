// Command loadgen drives the Stocky read API at a target request rate and
// records latency percentiles for each phase.
//
// It is deliberately dependency-free so Phase 0 can run with only the Go
// toolchain. The endpoint mix mirrors the baseline plan:
//
//	portfolio   70%
//	today       15%
//	stats       10%
//	historical   5%
//
// Requests are paced by giving each worker its own ticker. With N workers and
// a target rate R, each worker ticks every N/R seconds, so the aggregate rate
// is approximately R while still allowing up to N concurrent requests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type benchConfig struct {
	baseURL string
	users   int
	workers int
	timeout time.Duration
	seed    int64
}

type phaseResult struct {
	Name            string           `json:"name"`
	TargetRPS       float64          `json:"target_rps"`
	DurationSeconds float64          `json:"duration_seconds"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      time.Time        `json:"finished_at"`
	Requests        int64            `json:"requests"`
	Errors          int64            `json:"errors"`
	P50MS           float64          `json:"p50_ms"`
	P95MS           float64          `json:"p95_ms"`
	P99MS           float64          `json:"p99_ms"`
	MaxMS           float64          `json:"max_ms"`
	StatusCodes     map[string]int64 `json:"status_codes"`
	Endpoints       map[string]int64 `json:"endpoints"`
}

type workerResult struct {
	latencies []time.Duration
	errors    int64
	status    map[int]int64
	endpoints map[string]int64
}

type runSummary struct {
	BaseURL    string        `json:"base_url"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Users      int           `json:"users"`
	Workers    int           `json:"workers"`
	Phases     []phaseResult `json:"phases"`
}

func main() {
	baseURL := flag.String("base-url", "http://127.0.0.1:18080", "API base URL")
	users := flag.Int("users", 20000, "number of seeded users; request user IDs are 1..users")
	workers := flag.Int("workers", 512, "number of concurrent workers")
	warmup := flag.Duration("warmup", 60*time.Second, "warm-up phase duration")
	steady := flag.Duration("steady", 10*time.Minute, "steady phase duration (1M/hour target)")
	peak := flag.Duration("peak", 5*time.Minute, "peak phase duration")
	warmupRate := flag.Float64("warmup-rate", 50, "warm-up requests per second")
	steadyRate := flag.Float64("steady-rate", 278, "steady requests per second (1M/hour)")
	peakRate := flag.Float64("peak-rate", 1000, "peak requests per second")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request HTTP timeout")
	outDir := flag.String("out", "bench/results/latest", "directory for summary.json and summary.txt")
	seed := flag.Int64("seed", 42, "random seed")
	quick := flag.Bool("quick", false, "short smoke run: 30s warm-up, 2m steady, 1m peak")
	flag.Parse()

	if *quick {
		*warmup = 30 * time.Second
		*steady = 2 * time.Minute
		*peak = 1 * time.Minute
	}
	if *users < 1 {
		fmt.Fprintln(os.Stderr, "-users must be at least 1")
		os.Exit(2)
	}
	if *workers < 1 {
		fmt.Fprintln(os.Stderr, "-workers must be at least 1")
		os.Exit(2)
	}

	cfg := benchConfig{
		baseURL: strings.TrimRight(*baseURL, "/"),
		users:   *users,
		workers: *workers,
		timeout: *timeout,
		seed:    *seed,
	}

	transport := &http.Transport{
		MaxIdleConns:        *workers * 2,
		MaxIdleConnsPerHost: *workers,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.timeout,
	}

	phases := []struct {
		name     string
		rate     float64
		duration time.Duration
	}{
		{"warmup", *warmupRate, *warmup},
		{"steady", *steadyRate, *steady},
		{"peak", *peakRate, *peak},
	}

	startedAt := time.Now()
	results := make([]phaseResult, 0, len(phases))
	for _, phase := range phases {
		if phase.rate <= 0 || phase.duration <= 0 {
			continue
		}
		fmt.Printf("phase=%s target_rps=%.0f duration=%s\n", phase.name, phase.rate, phase.duration)
		result := runPhase(phase.name, phase.rate, phase.duration, cfg, client)
		results = append(results, result)
		fmt.Printf(
			"phase=%s requests=%d errors=%d p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms\n",
			result.Name, result.Requests, result.Errors,
			result.P50MS, result.P95MS, result.P99MS, result.MaxMS,
		)
	}

	summary := runSummary{
		BaseURL:    cfg.baseURL,
		StartedAt:  startedAt,
		FinishedAt: time.Now(),
		Users:      cfg.users,
		Workers:    cfg.workers,
		Phases:     results,
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create output directory: %v\n", err)
		os.Exit(1)
	}
	writeJSON(filepath.Join(*outDir, "summary.json"), summary)
	writeText(filepath.Join(*outDir, "summary.txt"), summary)
	fmt.Printf("results written to %s\n", *outDir)
}

func runPhase(name string, rate float64, duration time.Duration, cfg benchConfig, client *http.Client) phaseResult {
	result := phaseResult{
		Name:            name,
		TargetRPS:       rate,
		DurationSeconds: duration.Seconds(),
		StartedAt:       time.Now(),
		StatusCodes:     map[string]int64{},
		Endpoints:       map[string]int64{},
	}
	if rate <= 0 || duration <= 0 || cfg.workers <= 0 {
		result.FinishedAt = time.Now()
		return result
	}

	deadline := time.Now().Add(duration)
	results := make(chan workerResult, cfg.workers)

	var wg sync.WaitGroup
	wg.Add(cfg.workers)
	for i := 0; i < cfg.workers; i++ {
		go func(id int) {
			defer wg.Done()
			worker(id, cfg, rate, deadline, client, results)
		}(i)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var latencies []time.Duration
	for workerResult := range results {
		latencies = append(latencies, workerResult.latencies...)
		result.Errors += workerResult.errors
		for code, count := range workerResult.status {
			result.StatusCodes[strconv.Itoa(code)] += count
		}
		for endpoint, count := range workerResult.endpoints {
			result.Endpoints[endpoint] += count
		}
	}

	result.Requests = int64(len(latencies))
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		result.P50MS = durationMS(percentile(latencies, 50))
		result.P95MS = durationMS(percentile(latencies, 95))
		result.P99MS = durationMS(percentile(latencies, 99))
		result.MaxMS = durationMS(latencies[len(latencies)-1])
	}
	result.FinishedAt = time.Now()
	return result
}

func worker(id int, cfg benchConfig, rate float64, deadline time.Time, client *http.Client, out chan<- workerResult) {
	interval := time.Duration(float64(time.Second) * float64(cfg.workers) / rate)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	rng := rand.New(rand.NewSource(cfg.seed + int64(id)))
	result := workerResult{
		status:    map[int]int64{},
		endpoints: map[string]int64{},
	}

	// Stagger the first request from each worker so a phase does not begin with
	// a burst of cfg.workers simultaneous requests.
	if interval > time.Millisecond {
		time.Sleep(time.Duration(rng.Int63n(int64(interval))))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			out <- result
			return
		case <-ticker.C:
			if !time.Now().Before(deadline) {
				out <- result
				return
			}

			userID := 1 + rng.Intn(cfg.users)
			endpoint, path := pickEndpoint(rng, userID)
			start := time.Now()
			status, err := doRequest(client, cfg.baseURL+path)
			elapsed := time.Since(start)

			result.latencies = append(result.latencies, elapsed)
			result.endpoints[endpoint]++
			if err != nil {
				result.errors++
				result.status[0]++
				continue
			}
			result.status[status]++
			if status != http.StatusOK {
				result.errors++
			}
		}
	}
}

func pickEndpoint(rng *rand.Rand, userID int) (string, string) {
	switch n := rng.Intn(100); {
	case n < 70:
		return "portfolio", fmt.Sprintf("/api/v1/portfolio/%d", userID)
	case n < 85:
		return "today", fmt.Sprintf("/api/v1/today-stocks/%d", userID)
	case n < 95:
		return "stats", fmt.Sprintf("/api/v1/stats/%d", userID)
	default:
		return "historical", fmt.Sprintf("/api/v1/historical-inr/%d", userID)
	}
}

func doRequest(client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(sorted))*p/100.0)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func durationMS(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

func writeJSON(path string, summary runSummary) {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal summary: %v\n", err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
	}
}

func writeText(path string, summary runSummary) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Stocky Phase 0 baseline\n")
	fmt.Fprintf(&builder, "base_url: %s\n", summary.BaseURL)
	fmt.Fprintf(&builder, "started: %s\n", summary.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&builder, "finished: %s\n", summary.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&builder, "users: %d\n", summary.Users)
	fmt.Fprintf(&builder, "workers: %d\n\n", summary.Workers)

	fmt.Fprintf(&builder, "%-10s %10s %10s %10s %10s %10s %10s\n",
		"phase", "target", "requests", "errors", "p50_ms", "p95_ms", "p99_ms")
	for _, phase := range summary.Phases {
		fmt.Fprintf(&builder, "%-10s %10.0f %10d %10d %10.1f %10.1f %10.1f\n",
			phase.Name, phase.TargetRPS, phase.Requests, phase.Errors,
			phase.P50MS, phase.P95MS, phase.P99MS)
	}

	for _, phase := range summary.Phases {
		fmt.Fprintf(&builder, "\n[%s]\n", phase.Name)
		fmt.Fprintf(&builder, "status_codes: %v\n", phase.StatusCodes)
		fmt.Fprintf(&builder, "endpoints: %v\n", phase.Endpoints)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
	}
}
