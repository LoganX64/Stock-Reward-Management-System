package stocky

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LoganX64/stocky-api/internal/metrics"
	"github.com/gin-gonic/gin"
)

func newRateLimitedRouter(limiter *UserRateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(limiter.Middleware())
	router.GET("/api/v1/portfolio/:userId", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return router
}

func servePortfolio(router http.Handler, userID string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/"+userID, nil))
	return rec
}

func TestRateLimiterIsolatesUsersAndRejectsBurst(t *testing.T) {
	limiter := NewUserRateLimiter(1, 2, 100, nil)
	limiter.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	router := newRateLimitedRouter(limiter)

	first := servePortfolio(router, "1")
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-RateLimit-Limit"); got != "2" {
		t.Fatalf("X-RateLimit-Limit = %q, want 2", got)
	}
	if got := first.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1", got)
	}

	if rec := servePortfolio(router, "1"); rec.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", rec.Code)
	}

	rejected := servePortfolio(router, "1")
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("third request status = %d, want 429", rejected.Code)
	}
	if got := rejected.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("429 X-RateLimit-Remaining = %q, want 0", got)
	}
	if got := rejected.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("429 Retry-After = %q, want a positive value", got)
	}
	if !strings.Contains(rejected.Body.String(), "rate limit exceeded") {
		t.Fatalf("429 body = %q, want rate limit error", rejected.Body.String())
	}

	// Another user has an independent bucket and must not be affected.
	if rec := servePortfolio(router, "2"); rec.Code != http.StatusOK {
		t.Fatalf("user 2 status = %d, want 200", rec.Code)
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	limiter := NewUserRateLimiter(2, 1, 100, nil)
	now := time.Unix(1_700_000_000, 0)
	limiter.now = func() time.Time { return now }
	router := newRateLimitedRouter(limiter)

	if rec := servePortfolio(router, "7"); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	if rec := servePortfolio(router, "7"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}

	now = now.Add(500 * time.Millisecond)
	if rec := servePortfolio(router, "7"); rec.Code != http.StatusOK {
		t.Fatalf("request after refill status = %d, want 200", rec.Code)
	}
}

func TestRateLimiterDisabledPassesEverything(t *testing.T) {
	cases := map[string]*UserRateLimiter{
		"zero rate":  NewUserRateLimiter(0, 10, 100, nil),
		"zero burst": NewUserRateLimiter(10, 0, 100, nil),
	}
	for name, limiter := range cases {
		t.Run(name, func(t *testing.T) {
			router := newRateLimitedRouter(limiter)
			for i := 0; i < 20; i++ {
				rec := servePortfolio(router, "1")
				if rec.Code != http.StatusOK {
					t.Fatalf("request %d status = %d, want 200 when disabled", i, rec.Code)
				}
				if got := rec.Header().Get("X-RateLimit-Limit"); got != "" {
					t.Fatalf("disabled limiter set X-RateLimit-Limit = %q", got)
				}
			}
			if got := limiter.Len(); got != 0 {
				t.Fatalf("disabled limiter stored %d buckets, want 0", got)
			}
		})
	}
}

func TestRateLimiterEvictsOldestBucket(t *testing.T) {
	limiter := NewUserRateLimiter(1, 5, 2, nil)
	limiter.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	router := newRateLimitedRouter(limiter)

	for _, userID := range []string{"1", "2", "3"} {
		if rec := servePortfolio(router, userID); rec.Code != http.StatusOK {
			t.Fatalf("user %s status = %d, want 200", userID, rec.Code)
		}
	}

	if got := limiter.Len(); got != 2 {
		t.Fatalf("bucket count = %d, want maxKeys 2", got)
	}
	if _, ok := limiter.buckets["user:1"]; ok {
		t.Fatal("oldest bucket user:1 was not evicted")
	}
	if _, ok := limiter.buckets["user:3"]; !ok {
		t.Fatal("newest bucket user:3 is missing")
	}
}

func TestRateLimiterRecordsMetric(t *testing.T) {
	m := metrics.New()
	limiter := NewUserRateLimiter(1, 1, 100, m)
	limiter.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	router := newRateLimitedRouter(limiter)

	if rec := servePortfolio(router, "1"); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	if rec := servePortfolio(router, "1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	want := `stocky_http_rate_limited_total{route="/api/v1/portfolio/:userId"} 1`
	if !strings.Contains(string(body), want) {
		t.Fatalf("metrics output missing %q\n%s", want, body)
	}
}

func TestRateLimitKeyFallsBackToClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/health", nil)
	c.Request.RemoteAddr = "203.0.113.9:1234"
	if got := rateLimitKey(c); got != "ip:203.0.113.9" {
		t.Fatalf("rateLimitKey() = %q, want ip:203.0.113.9", got)
	}
}
