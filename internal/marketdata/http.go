package marketdata

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultHTTPTimeout   = 10 * time.Second
	DefaultMaxAttempts   = 3
	DefaultRetryBase     = time.Second
	DefaultMaxRetryDelay = 15 * time.Second
	DefaultMaxRetryAfter = 30 * time.Second
	DefaultUserAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36 Stocky/1.0"

	YahooBodyLimit    int64 = 1 << 20  // 1 MiB
	BhavcopyBodyLimit int64 = 32 << 20 // 32 MiB

	maxErrorSnippet = 512
)

// HTTPOptions controls the shared provider HTTP client.
type HTTPOptions struct {
	Timeout       time.Duration
	MaxAttempts   int
	RetryBase     time.Duration
	MaxRetryDelay time.Duration
	MaxRetryAfter time.Duration
	// RateLimit is the minimum spacing between requests. Zero disables the
	// client-side rate limiter.
	RateLimit time.Duration
	UserAgent string

	// The following hooks are for deterministic tests.
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
	Jitter func(time.Duration) time.Duration
}

// HTTPClient is the shared GET helper used by real market-data providers. It
// retries transient failures, honours Retry-After, enforces a response body
// limit, and optionally rate-limits requests across workers.
type HTTPClient struct {
	provider string
	client   *http.Client
	opts     HTTPOptions
	limiter  *rateLimiter
}

// NewHTTPClient builds a client. The provider name is included in errors and
// logs at call sites.
func NewHTTPClient(provider string, opts HTTPOptions) *HTTPClient {
	opts = normalizeHTTPOptions(opts)
	return &HTTPClient{
		provider: providerName(provider),
		client:   &http.Client{Timeout: opts.Timeout},
		opts:     opts,
		limiter:  newRateLimiter(opts.RateLimit, opts.Now, opts.Sleep),
	}
}

// Name returns the provider name attached to this client.
func (c *HTTPClient) Name() string {
	return c.provider
}

func normalizeHTTPOptions(opts HTTPOptions) HTTPOptions {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultHTTPTimeout
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = DefaultRetryBase
	}
	if opts.MaxRetryDelay <= 0 {
		opts.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if opts.MaxRetryAfter <= 0 {
		opts.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultUserAgent
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	if opts.Jitter == nil {
		opts.Jitter = jitter
	}
	return opts
}

// Get fetches rawURL. accept is sent as the Accept header. bodyLimit caps the
// response body; use YahooBodyLimit or BhavcopyBodyLimit for real providers.
func (c *HTTPClient) Get(ctx context.Context, rawURL, accept string, bodyLimit int64) ([]byte, error) {
	if bodyLimit <= 0 {
		bodyLimit = YahooBodyLimit
	}

	attempts := c.opts.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: build request: %w", c.provider, err)
		}
		req.Header.Set("User-Agent", c.opts.UserAgent)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}

		resp, err := c.client.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if attempt == attempts {
				return nil, fmt.Errorf("%s: GET %s: %w", c.provider, rawURL, err)
			}
			if err := c.sleepBeforeRetry(ctx, attempt, 0); err != nil {
				return nil, err
			}
			continue
		}

		body, truncated, readErr := readLimited(resp.Body, bodyLimit)
		_ = resp.Body.Close()

		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if attempt == attempts {
				return nil, fmt.Errorf("%s: read response: %w", c.provider, readErr)
			}
			if err := c.sleepBeforeRetry(ctx, attempt, 0); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			if truncated {
				return nil, &ValidationError{
					Provider: c.provider,
					Field:    "body",
					Reason:   fmt.Sprintf("response body exceeds %d bytes", bodyLimit),
				}
			}
			return body, nil
		}

		httpErr := &HTTPError{
			Provider:    c.provider,
			StatusCode:  resp.StatusCode,
			URL:         rawURL,
			BodySnippet: bodySnippet(body),
		}

		if !IsRetryableStatus(resp.StatusCode) || attempt == attempts {
			return nil, httpErr
		}

		retryAfter := time.Duration(0)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), c.opts.Now())
		}
		if err := c.sleepBeforeRetry(ctx, attempt, retryAfter); err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("%s: GET %s: request failed after %d attempts", c.provider, rawURL, attempts)
}

func (c *HTTPClient) sleepBeforeRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	delay := retryAfter
	if delay > 0 {
		if delay > c.opts.MaxRetryAfter {
			delay = c.opts.MaxRetryAfter
		}
	} else {
		delay = c.opts.RetryBase * time.Duration(1<<uint(attempt-1))
		if delay > c.opts.MaxRetryDelay {
			delay = c.opts.MaxRetryDelay
		}
		delay = c.opts.Jitter(delay)
		if delay > c.opts.MaxRetryDelay {
			delay = c.opts.MaxRetryDelay
		}
	}
	if delay < 0 {
		delay = 0
	}
	return c.opts.Sleep(ctx, delay)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.5 + rand.Float64()))
}

func readLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func bodySnippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > maxErrorSnippet {
		text = text[:maxErrorSnippet] + "..."
	}
	return text
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := when.Sub(now)
		if delay <= 0 {
			return 0
		}
		return delay
	}
	return 0
}

type rateLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func newRateLimiter(interval time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) *rateLimiter {
	return &rateLimiter{
		interval: interval,
		now:      now,
		sleep:    sleep,
	}
}

func (r *rateLimiter) Wait(ctx context.Context) error {
	if r == nil || r.interval <= 0 {
		return nil
	}

	r.mu.Lock()
	now := r.now()
	wait := time.Duration(0)
	if r.next.After(now) {
		wait = r.next.Sub(now)
	}
	r.next = now.Add(wait).Add(r.interval)
	r.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	return r.sleep(ctx, wait)
}
