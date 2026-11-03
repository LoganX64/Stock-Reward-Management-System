package stocky

import (
	"context"
	"net/http"
	"time"

	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
)

// RequestTimeout bounds the lifetime of every request context. Handlers that
// use QueryContext pass the deadline to PostgreSQL; if a handler finishes
// without writing a response, the middleware returns 503.
func RequestTimeout(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if timeout <= 0 {
			c.Next()
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		if ctx.Err() == context.DeadlineExceeded && !c.Writer.Written() {
			response.WriteJson(c.Writer, http.StatusServiceUnavailable, response.ErrorResponse("request timed out"))
		}
	}
}
