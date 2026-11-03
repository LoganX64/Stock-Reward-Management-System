package stocky

import (
	"container/list"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/LoganX64/stocky-api/internal/metrics"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
)

// UserRateLimiter is a per-instance token bucket keyed by userId (or client IP
// when a route has no userId parameter). The bucket map is bounded so a
// scanner that invents user IDs cannot grow memory without limit.
type UserRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	lru      list.List
	rate     float64
	burst    int
	maxKeys  int
	idleTTL  time.Duration
	now      func() time.Time
	metrics  *metrics.Metrics
	disabled bool
}

type tokenBucket struct {
	key      string
	tokens   float64
	last     time.Time
	lastSeen time.Time
	elem     *list.Element
}

// NewUserRateLimiter builds a read-path limiter. A non-positive rate or burst
// disables enforcement; the middleware then passes every request through.
func NewUserRateLimiter(rate float64, burst, maxKeys int, m *metrics.Metrics) *UserRateLimiter {
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &UserRateLimiter{
		buckets:  make(map[string]*tokenBucket),
		rate:     rate,
		burst:    burst,
		maxKeys:  maxKeys,
		idleTTL:  10 * time.Minute,
		now:      time.Now,
		metrics:  m,
		disabled: rate <= 0 || burst < 1,
	}
}

// Middleware enforces the bucket and returns 429 with Retry-After when the
// user has exhausted its burst. It is applied only to read routes.
func (l *UserRateLimiter) Middleware() gin.HandlerFunc {
	if l == nil || l.disabled {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		key := rateLimitKey(c)
		allowed, remaining, retry := l.allow(key)

		c.Header("X-RateLimit-Limit", strconv.Itoa(l.burst))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))

		if !allowed {
			retrySeconds := int(math.Ceil(retry.Seconds()))
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			c.Header("Retry-After", strconv.Itoa(retrySeconds))
			if l.metrics != nil {
				l.metrics.ObserveRateLimited(c.FullPath())
			}
			response.WriteJson(c.Writer, http.StatusTooManyRequests, response.ErrorResponse("rate limit exceeded"))
			c.Abort()
			return
		}

		c.Next()
	}
}

func (l *UserRateLimiter) allow(key string) (bool, int, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	bucket, ok := l.buckets[key]
	if !ok {
		bucket = &tokenBucket{
			key:      key,
			tokens:   float64(l.burst),
			last:     now,
			lastSeen: now,
		}
		bucket.elem = l.lru.PushFront(key)
		l.buckets[key] = bucket
	} else {
		elapsed := now.Sub(bucket.last).Seconds()
		if elapsed > 0 {
			bucket.tokens += elapsed * l.rate
			if bucket.tokens > float64(l.burst) {
				bucket.tokens = float64(l.burst)
			}
			bucket.last = now
		}
		bucket.lastSeen = now
		l.lru.MoveToFront(bucket.elem)
	}

	l.evictLocked(now)

	if bucket.tokens >= 1 {
		bucket.tokens--
		return true, int(bucket.tokens), 0
	}

	wait := time.Duration((1 - bucket.tokens) / l.rate * float64(time.Second))
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return false, 0, wait
}

func (l *UserRateLimiter) evictLocked(now time.Time) {
	for len(l.buckets) > l.maxKeys {
		oldest := l.lru.Back()
		if oldest == nil {
			return
		}
		key, _ := oldest.Value.(string)
		bucket, ok := l.buckets[key]
		if !ok {
			l.lru.Remove(oldest)
			continue
		}
		l.removeLocked(key, bucket)
	}

	// Periodically drop buckets that have been idle for a long time. The LRU
	// makes the scan bounded to entries that are already old.
	if len(l.buckets) == 0 || len(l.buckets)%1024 != 0 {
		return
	}
	for l.lru.Len() > 0 {
		oldest := l.lru.Back()
		key, _ := oldest.Value.(string)
		bucket, ok := l.buckets[key]
		if !ok {
			l.lru.Remove(oldest)
			continue
		}
		if now.Sub(bucket.lastSeen) < l.idleTTL {
			break
		}
		l.removeLocked(key, bucket)
	}
}

func (l *UserRateLimiter) removeLocked(key string, bucket *tokenBucket) {
	delete(l.buckets, key)
	if bucket != nil && bucket.elem != nil {
		l.lru.Remove(bucket.elem)
	}
}

// Len returns the number of live buckets, for tests and diagnostics.
func (l *UserRateLimiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// String makes accidental logging of a limiter readable without exposing the
// bucket map.
func (l *UserRateLimiter) String() string {
	if l == nil {
		return "<nil>"
	}
	return fmt.Sprintf("rate=%.2f/s burst=%d", l.rate, l.burst)
}
