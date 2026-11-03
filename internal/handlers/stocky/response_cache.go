package stocky

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/LoganX64/stocky-api/internal/metrics"
	"github.com/gin-gonic/gin"
)

const readCacheControl = "private, max-age=0, must-revalidate"

// ResponseCache gives read endpoints a strong ETag and, when a TTL is
// configured, a small per-instance response cache. The cache is intentionally
// private to one process: ETags are user-specific and a write on another API
// instance cannot invalidate this map directly. A short TTL bounds that
// staleness, and every local write clears the cache immediately.
type ResponseCache struct {
	mu         sync.Mutex
	entries    map[string]*responseCacheEntry
	lru        list.List
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	metrics    *metrics.Metrics
}

type responseCacheEntry struct {
	key      string
	userID   string
	etag     string
	status   int
	header   http.Header
	body     []byte
	storedAt time.Time
	elem     *list.Element
}

// NewResponseCache builds an ETag cache. A non-positive TTL or maxEntries
// disables response storage while leaving ETag and 304 handling enabled.
func NewResponseCache(ttl time.Duration, maxEntries int, m *metrics.Metrics) *ResponseCache {
	if maxEntries < 0 {
		maxEntries = 0
	}
	return &ResponseCache{
		entries:    make(map[string]*responseCacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		metrics:    m,
	}
}

// Middleware wraps one read handler. It computes the ETag from the exact JSON
// bytes returned by the handler. On a hit it answers without invoking the
// handler, so a matching conditional request does not touch PostgreSQL.
func (rc *ResponseCache) Middleware(next gin.HandlerFunc) gin.HandlerFunc {
	if rc == nil {
		return next
	}
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			next(c)
			return
		}

		key := requestCacheKey(c)
		if entry, ok := rc.get(key); ok {
			if etagMatches(c.GetHeader("If-None-Match"), entry.etag) {
				rc.observe("revalidated")
				writeNotModified(c.Writer, entry.etag)
				c.Abort()
				return
			}

			rc.observe("hit")
			writeCachedResponse(c.Writer, entry)
			c.Abort()
			return
		}

		if rc.cacheEnabled() {
			rc.observe("miss")
		}

		original := c.Writer
		buffered := &bufferedResponseWriter{ResponseWriter: original}
		c.Writer = buffered

		// Restore the original writer even if the handler panics. Without this,
		// gin's Recovery middleware would write its 500 into the buffer and the
		// client would receive an empty response.
		func() {
			defer func() {
				c.Writer = original
				if recovered := recover(); recovered != nil {
					panic(recovered)
				}
			}()
			next(c)
		}()

		status := buffered.status
		if status == 0 {
			status = http.StatusOK
		}
		body := buffered.body.Bytes()
		headers := cloneHeader(original.Header())

		if status != http.StatusOK {
			writeCapturedResponse(original, status, headers, body)
			return
		}

		etag := strongETag(body)
		headers.Set("ETag", etag)
		headers.Set("Cache-Control", readCacheControl)

		if etagMatches(c.GetHeader("If-None-Match"), etag) {
			rc.observe("revalidated")
			writeNotModified(original, etag)
			return
		}

		rc.store(key, c.Param("userId"), etag, status, headers, body)
		writeCapturedResponse(original, status, headers, body)
	}
}

// InvalidateUser removes every cached response for one user. It is called after
// a successful reward or adjustment write on this instance.
func (rc *ResponseCache) InvalidateUser(userID string) {
	if rc == nil || userID == "" {
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	for key, entry := range rc.entries {
		if entry.userID == userID {
			rc.removeLocked(key, entry)
		}
	}
	rc.publishSizeLocked()
}

// InvalidateAll clears the cache. It is useful when a write path cannot cheaply
// identify the affected user (for example a corporate-action refresh).
func (rc *ResponseCache) InvalidateAll() {
	if rc == nil {
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.entries = make(map[string]*responseCacheEntry)
	rc.lru.Init()
	rc.publishSizeLocked()
}

// Len returns the current number of cached responses.
func (rc *ResponseCache) Len() int {
	if rc == nil {
		return 0
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.entries)
}

func (rc *ResponseCache) cacheEnabled() bool {
	return rc != nil && rc.ttl > 0 && rc.maxEntries > 0
}

func (rc *ResponseCache) get(key string) (*responseCacheEntry, bool) {
	if !rc.cacheEnabled() {
		return nil, false
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	entry, ok := rc.entries[key]
	if !ok {
		return nil, false
	}
	if rc.now().Sub(entry.storedAt) >= rc.ttl {
		rc.removeLocked(key, entry)
		rc.publishSizeLocked()
		return nil, false
	}

	rc.lru.MoveToFront(entry.elem)
	return entry, true
}

func (rc *ResponseCache) store(key, userID, etag string, status int, header http.Header, body []byte) {
	if !rc.cacheEnabled() {
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	if existing, ok := rc.entries[key]; ok {
		rc.removeLocked(key, existing)
	}

	entry := &responseCacheEntry{
		key:      key,
		userID:   userID,
		etag:     etag,
		status:   status,
		header:   header,
		body:     append([]byte(nil), body...),
		storedAt: rc.now(),
	}
	entry.elem = rc.lru.PushFront(key)
	rc.entries[key] = entry

	for len(rc.entries) > rc.maxEntries {
		oldest := rc.lru.Back()
		if oldest == nil {
			break
		}
		oldestKey, _ := oldest.Value.(string)
		oldestEntry, ok := rc.entries[oldestKey]
		if !ok {
			rc.lru.Remove(oldest)
			continue
		}
		rc.removeLocked(oldestKey, oldestEntry)
		rc.observeLocked("evicted")
	}
	rc.publishSizeLocked()
}

func (rc *ResponseCache) removeLocked(key string, entry *responseCacheEntry) {
	delete(rc.entries, key)
	if entry != nil && entry.elem != nil {
		rc.lru.Remove(entry.elem)
	}
}

func (rc *ResponseCache) publishSizeLocked() {
	if rc.metrics != nil {
		rc.metrics.SetHTTPCacheEntries(len(rc.entries))
	}
}

func (rc *ResponseCache) observe(result string) {
	if rc == nil || rc.metrics == nil {
		return
	}
	rc.metrics.ObserveHTTPCache(result)
}

func (rc *ResponseCache) observeLocked(result string) {
	if rc.metrics != nil {
		rc.metrics.ObserveHTTPCache(result)
	}
}

// bufferedResponseWriter captures handler output without committing it to the
// client. The response cache needs the body before it can decide between a 200
// and a 304.
type bufferedResponseWriter struct {
	gin.ResponseWriter
	body   bytes.Buffer
	status int
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
}

func (w *bufferedResponseWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *bufferedResponseWriter) WriteString(value string) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.WriteString(value)
}

func (w *bufferedResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *bufferedResponseWriter) Size() int {
	return w.body.Len()
}

func (w *bufferedResponseWriter) Written() bool {
	return w.status != 0
}

func requestCacheKey(c *gin.Context) string {
	path := c.Request.URL.EscapedPath()
	if query := c.Request.URL.Query().Encode(); query != "" {
		return path + "?" + query
	}
	return path
}

func strongETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// etagMatches implements the weak comparison used by If-None-Match for GET.
// A strong ETag may be matched by either a strong or weak candidate.
func etagMatches(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.TrimSpace(candidate)
		if candidate == etag {
			return true
		}
	}
	return false
}

func writeNotModified(w gin.ResponseWriter, etag string) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", readCacheControl)
	w.WriteHeader(http.StatusNotModified)
}

func writeCachedResponse(w gin.ResponseWriter, entry *responseCacheEntry) {
	applyHeader(w.Header(), entry.header)
	w.WriteHeader(entry.status)
	if len(entry.body) > 0 {
		_, _ = w.Write(entry.body)
	}
}

func writeCapturedResponse(w gin.ResponseWriter, status int, header http.Header, body []byte) {
	applyHeader(w.Header(), header)
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func applyHeader(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

// rateLimitKey returns the per-user key for a read route. Routes without a
// userId parameter fall back to the client address.
func rateLimitKey(c *gin.Context) string {
	if userID := c.Param("userId"); userID != "" {
		return "user:" + userID
	}
	if ip := c.ClientIP(); ip != "" {
		return "ip:" + ip
	}
	return "unknown"
}
