package stocky

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestETagCacheReturns304WithoutCallingHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 10, nil)
	var calls int32

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.JSON(http.StatusOK, gin.H{"userId": c.Param("userId"), "value": 42})
	}))

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/7", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response has no ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/7", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("second status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 body length = %d, want 0", second.Body.Len())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (cache should answer the 304)", got)
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}
	if got := second.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Fatalf("Cache-Control = %q, want private", got)
	}
}

func TestETagCacheSeparatesUsersAndQueries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 10, nil)
	var calls int32

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.JSON(http.StatusOK, gin.H{
			"userId": c.Param("userId"),
			"limit":  c.Query("limit"),
		})
	}))

	for _, path := range []string{
		"/api/v1/portfolio/1?limit=10",
		"/api/v1/portfolio/1?limit=20",
		"/api/v1/portfolio/2?limit=10",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("handler calls = %d, want 3 distinct cache keys", got)
	}
}

func TestETagWithoutCacheStillRevalidates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(0, 0, nil)
	var calls int32

	router := gin.New()
	router.GET("/api/v1/stats/:userId", cache.Middleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.JSON(http.StatusOK, gin.H{"value": 1})
	}))

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/stats/1", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response has no ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/1", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("second status = %d, want 304", second.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler calls = %d, want 2 when caching is disabled", got)
	}
}

func TestETagChangesWhenResponseChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(0, 0, nil)
	value := "first"

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"value": value})
	}))

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil))
	firstETag := first.Header().Get("ETag")

	value = "second"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil)
	req.Header.Set("If-None-Match", firstETag)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, req)

	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when the body changes", second.Code)
	}
	if second.Header().Get("ETag") == firstETag {
		t.Fatal("ETag did not change with the response body")
	}
}

func TestETagCacheInvalidatesOneUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 10, nil)
	var calls int32

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.JSON(http.StatusOK, gin.H{"userId": c.Param("userId")})
	}))

	for _, path := range []string{"/api/v1/portfolio/1", "/api/v1/portfolio/2"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler calls = %d, want 2", got)
	}

	cache.InvalidateUser("1")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil))
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("handler calls after invalidation = %d, want 3", got)
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/2", nil))
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("unrelated user cache was invalidated: calls = %d, want 3", got)
	}
}

func TestETagCacheEvictsLeastRecentlyUsed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 1, nil)

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"userId": c.Param("userId")})
	}))

	for _, path := range []string{"/api/v1/portfolio/1", "/api/v1/portfolio/2"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	if got := cache.Len(); got != 1 {
		t.Fatalf("cache length = %d, want 1", got)
	}
}

func TestETagCacheLetsRecoveryWritePanicResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 10, nil)

	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		panic("handler failure")
	}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 after panic", rec.Code)
	}
	if got := cache.Len(); got != 0 {
		t.Fatalf("cache length = %d, want 0 after panic", got)
	}
}

func TestEtagMatches(t *testing.T) {
	etag := `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{etag, true},
		{`W/"abc123"`, true},
		{`"other", "abc123"`, true},
		{"*", true},
		{`"other"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, etag); got != tc.want {
			t.Errorf("etagMatches(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestETagCacheDoesNotCacheErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 10, nil)
	var calls int32

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failure %d", calls)})
	}))

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	}
	if got := cache.Len(); got != 0 {
		t.Fatalf("cache length = %d, want 0 for errors", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler calls = %d, want 2", got)
	}
}

func TestResponseCacheExpiresEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := NewResponseCache(time.Minute, 10, nil)
	now := time.Now()
	cache.now = func() time.Time { return now }
	var calls int32

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", cache.Middleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.JSON(http.StatusOK, gin.H{"userId": c.Param("userId")})
	}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil))
	if got := cache.Len(); got != 1 {
		t.Fatalf("cache length = %d, want 1", got)
	}

	now = now.Add(2 * time.Minute)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil))
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler calls = %d, want 2 after expiry", got)
	}
}
