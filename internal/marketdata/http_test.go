package marketdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testHTTPOptions() HTTPOptions {
	return HTTPOptions{
		Timeout:       2 * time.Second,
		MaxAttempts:   3,
		RetryBase:     time.Millisecond,
		MaxRetryDelay: 5 * time.Millisecond,
		MaxRetryAfter: 30 * time.Second,
		Jitter:        func(d time.Duration) time.Duration { return d },
	}
}

func TestHTTPClientGetSuccess(t *testing.T) {
	var gotUserAgent, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	options := testHTTPOptions()
	options.UserAgent = "stocky-test-agent"
	client := NewHTTPClient("test", options)

	body, err := client.Get(context.Background(), server.URL, "application/json", 1024)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("Get() body = %q, want ok", body)
	}
	if gotUserAgent != "stocky-test-agent" {
		t.Errorf("User-Agent = %q, want stocky-test-agent", gotUserAgent)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

func TestHTTPClientRetriesTransientStatus(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&attempts, 1)
		if current < 3 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	var sleeps []time.Duration
	options := testHTTPOptions()
	options.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	client := NewHTTPClient("test", options)

	body, err := client.Get(context.Background(), server.URL, "", 1024)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("Get() body = %q, want ok", body)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if len(sleeps) != 2 {
		t.Fatalf("sleep count = %d, want 2", len(sleeps))
	}
	if sleeps[0] != time.Millisecond || sleeps[1] != 2*time.Millisecond {
		t.Errorf("sleeps = %v, want [1ms 2ms]", sleeps)
	}
}

func TestHTTPClientDoesNotRetry400(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := NewHTTPClient("test", testHTTPOptions())
	_, err := client.Get(context.Background(), server.URL, "", 1024)
	if err == nil {
		t.Fatal("Get() accepted HTTP 400")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("Get() error = %v, want HTTPError 400", err)
	}
	if httpErr.BodySnippet != "bad request" {
		t.Errorf("BodySnippet = %q, want bad request", httpErr.BodySnippet)
	}
}

func TestHTTPClientExhaustsRetries(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	var sleeps []time.Duration
	options := testHTTPOptions()
	options.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	client := NewHTTPClient("test", options)

	_, err := client.Get(context.Background(), server.URL, "", 1024)
	if err == nil {
		t.Fatal("Get() accepted repeated HTTP 500")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if len(sleeps) != 2 {
		t.Errorf("sleep count = %d, want 2", len(sleeps))
	}
	if !IsHTTPError(err) {
		t.Errorf("Get() error = %T, want *HTTPError", err)
	}
}

func TestHTTPClientHonorsRetryAfter(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	var sleeps []time.Duration
	options := testHTTPOptions()
	options.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	client := NewHTTPClient("test", options)

	body, err := client.Get(context.Background(), server.URL, "", 1024)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("Get() body = %q, want ok", body)
	}
	if len(sleeps) != 1 || sleeps[0] != 2*time.Second {
		t.Errorf("sleeps = %v, want [2s]", sleeps)
	}
}

func TestHTTPClientCapsRetryAfter(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "120")
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	var sleeps []time.Duration
	options := testHTTPOptions()
	options.MaxRetryAfter = 30 * time.Second
	options.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	client := NewHTTPClient("test", options)

	if _, err := client.Get(context.Background(), server.URL, "", 1024); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(sleeps) != 1 || sleeps[0] != 30*time.Second {
		t.Errorf("sleeps = %v, want [30s]", sleeps)
	}
}

func TestHTTPClientBodyLimit(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		_, _ = w.Write([]byte("0123456789abcdef"))
	}))
	defer server.Close()

	client := NewHTTPClient("test", testHTTPOptions())
	_, err := client.Get(context.Background(), server.URL, "", 8)
	if err == nil {
		t.Fatal("Get() accepted an oversized body")
	}
	if !IsValidationError(err) {
		t.Fatalf("Get() error = %T, want *ValidationError", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestHTTPClientContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	options := testHTTPOptions()
	options.MaxAttempts = 1
	client := NewHTTPClient("test", options)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := client.Get(ctx, server.URL, "", 1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get() error = %v, want context.Canceled", err)
	}
}

func TestHTTPClientRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	now := time.Unix(1000, 0)
	var sleeps []time.Duration
	options := testHTTPOptions()
	options.MaxAttempts = 1
	options.RateLimit = time.Second
	options.Now = func() time.Time { return now }
	options.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		now = now.Add(d)
		return nil
	}
	client := NewHTTPClient("test", options)

	if _, err := client.Get(context.Background(), server.URL, "", 1024); err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("first Get() slept %v, want none", sleeps)
	}
	if _, err := client.Get(context.Background(), server.URL, "", 1024); err != nil {
		t.Fatalf("second Get() error = %v", err)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Errorf("sleeps = %v, want [1s]", sleeps)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.November, 2, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "seconds", value: "2", want: 2 * time.Second},
		{name: "zero", value: "0", want: 0},
		{name: "negative", value: "-1", want: 0},
		{name: "empty", value: "", want: 0},
		{name: "invalid", value: "soon", want: 0},
		{name: "http date", value: now.Add(5 * time.Second).Format(http.TimeFormat), want: 5 * time.Second},
		{name: "past http date", value: now.Add(-5 * time.Second).Format(http.TimeFormat), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{status: 408, want: true},
		{status: 429, want: true},
		{status: 500, want: true},
		{status: 503, want: true},
		{status: 400, want: false},
		{status: 401, want: false},
		{status: 404, want: false},
		{status: 200, want: false},
	}

	for _, tt := range tests {
		if got := IsRetryableStatus(tt.status); got != tt.want {
			t.Errorf("IsRetryableStatus(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestHTTPErrorIncludesSnippet(t *testing.T) {
	err := &HTTPError{Provider: "yahoo", StatusCode: 500, URL: "https://example.test", BodySnippet: "boom"}
	if got := err.Error(); got == "" || !strings.Contains(got, "boom") {
		t.Errorf("HTTPError.Error() = %q, want it to include boom", got)
	}
}
