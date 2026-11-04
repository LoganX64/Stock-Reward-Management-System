// Command lb is a small local load balancer for testing multiple stateless
// Stocky API instances. It is intentionally dependency-free and is not part of
// the production deployment; Docker Compose uses nginx (see deploy/nginx.conf).
//
// Two strategies are available:
//
//	round-robin  spread every request across healthy API instances
//	user         hash the trailing userId path segment to one healthy instance
//
// The user strategy is useful for measuring the per-instance ETag/LRU cache:
// repeated reads for one user stay on the same instance and can hit its cache.
// Round-robin shows the multi-instance behaviour where each cache is cold and
// each rate limiter has its own bucket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type backend struct {
	url     *url.URL
	proxy   *httputil.ReverseProxy
	healthy atomic.Bool
}

type loadBalancer struct {
	backends   []*backend
	strategy   string
	healthPath string
	next       atomic.Uint64
	transport  *http.Transport
}

// newProxyTransport returns a transport tuned for a local benchmark. The
// default transport keeps only two idle connections per host, so at thousands
// of requests per second the proxy closes and reopens backend sockets
// constantly and exhausts ephemeral ports on Windows. One shared transport
// keeps the pooled connections alive for every backend.
func newProxyTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   1024,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
}

func newLoadBalancer(rawBackends []string, strategy, healthPath string) (*loadBalancer, error) {
	if len(rawBackends) == 0 {
		return nil, fmt.Errorf("at least one -backends URL is required")
	}
	if strategy != "round-robin" && strategy != "user" {
		return nil, fmt.Errorf("unsupported -strategy %q (want round-robin or user)", strategy)
	}
	if healthPath == "" {
		healthPath = "/health"
	}

	lb := &loadBalancer{strategy: strategy, healthPath: healthPath, transport: newProxyTransport()}
	for _, raw := range rawBackends {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		target, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse backend %q: %w", raw, err)
		}
		if target.Scheme != "http" && target.Scheme != "https" {
			return nil, fmt.Errorf("backend %q must use http or https", raw)
		}
		if target.Host == "" {
			return nil, fmt.Errorf("backend %q has no host", raw)
		}

		b := &backend{url: target}
		b.proxy = httputil.NewSingleHostReverseProxy(target)
		b.proxy.Transport = lb.transport
		b.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			// A failed proxy attempt usually means the instance stopped between
			// the health check and the request. Mark it down; the periodic
			// probe restores it.
			b.healthy.Store(false)
			log.Printf("backend %s failed: %v", b.url, err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
		lb.backends = append(lb.backends, b)
	}

	if len(lb.backends) == 0 {
		return nil, fmt.Errorf("no usable backend URLs")
	}
	return lb, nil
}

func (lb *loadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		if lb.healthyCount() == 0 {
			http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
		return
	case "/lb/backends":
		lb.writeBackendStatus(w)
		return
	}

	b := lb.pick(r)
	if b == nil {
		http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
		return
	}
	b.proxy.ServeHTTP(w, r)
}

func (lb *loadBalancer) pick(r *http.Request) *backend {
	healthy := lb.healthyBackends()
	if len(healthy) == 0 {
		return nil
	}

	if lb.strategy == "user" {
		if key := userKey(r.URL.Path); key != "" {
			h := fnv.New32a()
			_, _ = h.Write([]byte(key))
			return healthy[int(h.Sum32())%len(healthy)]
		}
	}

	index := lb.next.Add(1) - 1
	return healthy[int(index%uint64(len(healthy)))]
}

func (lb *loadBalancer) healthyBackends() []*backend {
	healthy := make([]*backend, 0, len(lb.backends))
	for _, b := range lb.backends {
		if b.healthy.Load() {
			healthy = append(healthy, b)
		}
	}
	return healthy
}

func (lb *loadBalancer) healthyCount() int {
	count := 0
	for _, b := range lb.backends {
		if b.healthy.Load() {
			count++
		}
	}
	return count
}

func (lb *loadBalancer) writeBackendStatus(w http.ResponseWriter) {
	type status struct {
		URL     string `json:"url"`
		Healthy bool   `json:"healthy"`
	}
	out := make([]status, 0, len(lb.backends))
	for _, b := range lb.backends {
		out = append(out, status{URL: b.url.String(), Healthy: b.healthy.Load()})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"strategy": lb.strategy,
		"backends": out,
	})
}

func (lb *loadBalancer) probeAll(ctx context.Context, client *http.Client) {
	var wg sync.WaitGroup
	for _, b := range lb.backends {
		wg.Add(1)
		go func(b *backend) {
			defer wg.Done()
			lb.probe(ctx, client, b)
		}(b)
	}
	wg.Wait()
}

func (lb *loadBalancer) probe(ctx context.Context, client *http.Client, b *backend) {
	target := *b.url
	target.Path = strings.TrimRight(target.Path, "/") + lb.healthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		b.healthy.Store(false)
		return
	}

	resp, err := client.Do(req)
	wasHealthy := b.healthy.Swap(err == nil && resp != nil && resp.StatusCode == http.StatusOK)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if wasHealthy != b.healthy.Load() {
		if b.healthy.Load() {
			log.Printf("backend %s is healthy", b.url)
		} else if err != nil {
			log.Printf("backend %s is unhealthy: %v", b.url, err)
		} else {
			log.Printf("backend %s is unhealthy: status %d", b.url, resp.StatusCode)
		}
	}
}

func (lb *loadBalancer) healthLoop(ctx context.Context, interval, timeout time.Duration) {
	client := &http.Client{Timeout: timeout}
	lb.probeAll(ctx, client)
	if lb.healthyCount() == 0 {
		log.Printf("warning: no healthy backends at startup; retrying every %s", interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lb.probeAll(ctx, client)
		}
	}
}

// userKey extracts the userId from the read routes that scope data by user.
// Other paths (writes, health, metrics, adjustments) fall back to the selected
// strategy so a numeric adjustment ID is never treated as a user ID.
func userKey(path string) string {
	path = strings.TrimRight(path, "/")
	for _, prefix := range []string{"/portfolio/", "/stats/", "/today-stocks/", "/historical-inr/"} {
		index := strings.LastIndex(path, prefix)
		if index < 0 {
			continue
		}
		segment := path[index+len(prefix):]
		if segment == "" || strings.Contains(segment, "/") {
			return ""
		}
		return segment
	}
	return ""
}

func parseBackends(raw string) []string {
	return strings.Split(raw, ",")
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18081", "address for the load balancer")
	backends := flag.String("backends", "http://127.0.0.1:18080", "comma-separated API base URLs")
	strategy := flag.String("strategy", "round-robin", "routing strategy: round-robin or user")
	healthPath := flag.String("health-path", "/health", "backend health-check path")
	healthInterval := flag.Duration("health-interval", 5*time.Second, "health-check interval")
	healthTimeout := flag.Duration("health-timeout", 2*time.Second, "health-check timeout")
	shutdownTimeout := flag.Duration("shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
	flag.Parse()

	lb, err := newLoadBalancer(parseBackends(*backends), *strategy, *healthPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load balancer: %v\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go lb.healthLoop(ctx, *healthInterval, *healthTimeout)

	server := &http.Server{
		Addr:              *listen,
		Handler:           lb,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("load balancer listening on %s strategy=%s backends=%s", *listen, *strategy, *backends)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
