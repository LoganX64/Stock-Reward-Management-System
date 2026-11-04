package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func markAllHealthy(lb *loadBalancer) {
	for _, b := range lb.backends {
		b.healthy.Store(true)
	}
}

func TestNewLoadBalancerValidatesInput(t *testing.T) {
	cases := []struct {
		name     string
		backends []string
		strategy string
	}{
		{"no backends", nil, "round-robin"},
		{"invalid url", []string{"://bad"}, "round-robin"},
		{"missing scheme", []string{"127.0.0.1:8080"}, "round-robin"},
		{"bad strategy", []string{"http://127.0.0.1:8080"}, "random"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newLoadBalancer(tc.backends, tc.strategy, "/health"); err == nil {
				t.Fatal("newLoadBalancer() error = nil, want validation error")
			}
		})
	}
}

func TestRoundRobinCyclesThroughHealthyBackends(t *testing.T) {
	lb, err := newLoadBalancer([]string{"http://api-a:8080", "http://api-b:8080"}, "round-robin", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	markAllHealthy(lb)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil)
	got := []string{
		lb.pick(req).url.Host,
		lb.pick(req).url.Host,
		lb.pick(req).url.Host,
	}
	want := []string{"api-a:8080", "api-b:8080", "api-a:8080"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pick sequence = %v, want %v", got, want)
		}
	}
}

func TestRoundRobinSkipsUnhealthyBackends(t *testing.T) {
	lb, err := newLoadBalancer([]string{"http://api-a:8080", "http://api-b:8080"}, "round-robin", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	lb.backends[0].healthy.Store(false)
	lb.backends[1].healthy.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil)
	for i := 0; i < 4; i++ {
		if got := lb.pick(req).url.Host; got != "api-b:8080" {
			t.Fatalf("pick() = %q, want the only healthy backend", got)
		}
	}
}

func TestUserStrategyIsStablePerUser(t *testing.T) {
	lb, err := newLoadBalancer([]string{"http://api-a:8080", "http://api-b:8080"}, "user", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	markAllHealthy(lb)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/42", nil)
	first := lb.pick(req).url.Host
	for i := 0; i < 20; i++ {
		if got := lb.pick(req).url.Host; got != first {
			t.Fatalf("user routing is not stable: got %q, first %q", got, first)
		}
	}

	seen := map[string]bool{}
	for user := 1; user <= 40; user++ {
		userReq := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/"+strconv.Itoa(user), nil)
		seen[lb.pick(userReq).url.Host] = true
	}
	if len(seen) != 2 {
		t.Fatalf("user strategy only used %d of 2 backends", len(seen))
	}
}

func TestUserKey(t *testing.T) {
	cases := map[string]string{
		"/api/v1/portfolio/42":     "42",
		"/api/v1/stats/7/":         "7",
		"/api/v1/today-stocks/1":   "1",
		"/api/v1/historical-inr/9": "9",
		"/api/v1/reward":           "",
		"/api/v1/adjustments/5":    "",
		"/health":                  "",
		"/metrics":                 "",
	}
	for path, want := range cases {
		if got := userKey(path); got != want {
			t.Errorf("userKey(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestHealthEndpointReflectsBackendState(t *testing.T) {
	lb, err := newLoadBalancer([]string{"http://api-a:8080"}, "round-robin", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health without backends = %d, want 503", rec.Code)
	}

	lb.backends[0].healthy.Store(true)
	rec = httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health with one backend = %d, want 200", rec.Code)
	}
}

func TestStatusEndpointListsBackends(t *testing.T) {
	lb, err := newLoadBalancer([]string{"http://api-a:8080", "http://api-b:8080"}, "user", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	lb.backends[0].healthy.Store(true)

	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lb/backends", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"http://api-a:8080"`, `"http://api-b:8080"`, `"healthy":true`, `"healthy":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status body missing %q: %s", want, body)
		}
	}
}

func TestProxyForwardsToBackend(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "A:"+r.URL.Path)
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "B:"+r.URL.Path)
	}))
	defer backendB.Close()

	lb, err := newLoadBalancer([]string{backendA.URL, backendB.URL}, "round-robin", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	markAllHealthy(lb)

	var got []string
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		lb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/42", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rec.Code)
		}
		got = append(got, rec.Body.String())
	}

	want := []string{"A:/api/v1/portfolio/42", "B:/api/v1/portfolio/42", "A:/api/v1/portfolio/42", "B:/api/v1/portfolio/42"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("response sequence = %v, want %v", got, want)
		}
	}
}

func TestProxyUsesSharedPooledTransport(t *testing.T) {
	lb, err := newLoadBalancer([]string{"http://api-a:8080", "http://api-b:8080"}, "round-robin", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	if lb.transport == nil {
		t.Fatal("newLoadBalancer() transport = nil, want a pooled transport")
	}
	if lb.transport.MaxIdleConnsPerHost < 256 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want at least 256 for a load test", lb.transport.MaxIdleConnsPerHost)
	}
	for _, b := range lb.backends {
		if b.proxy.Transport != lb.transport {
			t.Fatalf("backend %s does not use the shared transport", b.url)
		}
	}
}

func TestProbeAllMarksBackendsHealthy(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	lb, err := newLoadBalancer([]string{healthy.URL, unhealthy.URL}, "round-robin", "/health")
	if err != nil {
		t.Fatalf("newLoadBalancer() error = %v", err)
	}
	lb.probeAll(context.Background(), &http.Client{Timeout: time.Second})

	if !lb.backends[0].healthy.Load() {
		t.Error("healthy backend was not marked healthy")
	}
	if lb.backends[1].healthy.Load() {
		t.Error("unhealthy backend was marked healthy")
	}
}
