package stocky

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// generateRequestID creates a short, collision-resistant request ID
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// RequestIDLogger middleware
func RequestIDLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := generateRequestID()
		c.Set("request_id", reqID)

		entry := logrus.WithField("request_id", reqID)
		c.Set("logger", entry)

		c.Next()
	}
}

// requestID returns the request ID from context
func requestID(c *gin.Context) string {
	if id, exists := c.Get("request_id"); exists {
		if str, ok := id.(string); ok {
			return str
		}
	}
	return "unknown"
}

// getLogger returns a logger with request_id already attached
func getLogger(c *gin.Context) *logrus.Entry {
	if logger, exists := c.Get("logger"); exists {
		if entry, ok := logger.(*logrus.Entry); ok {
			return entry
		}
	}
	return logrus.WithField("request_id", requestID(c))
}

// parseUserID extracts and validates userId
func parseUserID(c *gin.Context) (int, bool) {
	idStr := c.Param("userId")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest,
			response.ErrorResponse("invalid userId - must be a positive integer"))
		return 0, false
	}
	return id, true
}

// parseRewardID extracts and validates reward ID
func parseRewardID(c *gin.Context) (int, bool) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest,
			response.ErrorResponse("invalid reward ID - must be a positive integer"))
		return 0, false
	}
	return id, true
}

func parsePagination(c *gin.Context) (int, int, bool) {
	const (
		defaultLimit = 100
		maxLimit     = 500
	)

	limit := defaultLimit
	offset := 0

	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			response.WriteJson(c.Writer, http.StatusBadRequest,
				response.ErrorResponse("invalid limit - must be a positive integer"))
			return 0, 0, false
		}
		if parsed > maxLimit {
			parsed = maxLimit
		}
		limit = parsed
	}

	if rawOffset := c.Query("offset"); rawOffset != "" {
		parsed, err := strconv.Atoi(rawOffset)
		if err != nil || parsed < 0 {
			response.WriteJson(c.Writer, http.StatusBadRequest,
				response.ErrorResponse("invalid offset - must be zero or a positive integer"))
			return 0, 0, false
		}
		offset = parsed
	}

	return limit, offset, true
}

// HealthRoute registers the liveness endpoint. Worker-only processes expose it
// to Docker/orchestrator health checks without mounting the API routes.
func HealthRoute(r *gin.Engine) {
	r.GET("/health", func(c *gin.Context) {
		getLogger(c).Info("Health check endpoint hit")
		response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
			"status": "OK",
		})
	})
}

// Routes now accepts the handler
func Routes(r *gin.Engine, handler *Handler) {
	r.Use(RequestIDLogger())

	HealthRoute(r)

	// API v1 group
	v1 := r.Group("/api/v1")
	{
		v1.POST("/reward", handler.CreateReward)
		v1.POST("/adjustments/:id", handler.adjustmentHandler)

		// Read endpoints are rate-limited per user. Portfolio and stats also
		// answer conditional requests from the local ETag cache; the other
		// reads stay uncached because their query space is larger.
		reads := v1.Group("")
		if handler.rateLimiter != nil {
			reads.Use(handler.rateLimiter.Middleware())
		}
		reads.GET("/today-stocks/:userId", handler.GetTodayStocks)
		reads.GET("/historical-inr/:userId", handler.GetHistoricalINR)
		reads.GET("/stats/:userId", handler.readHandler(handler.StatsHandler))
		reads.GET("/portfolio/:userId", handler.readHandler(handler.PortfolioHandler))
	}
}

// readHandler applies the optional ETag/response cache to one read handler.
func (h *Handler) readHandler(next gin.HandlerFunc) gin.HandlerFunc {
	if h.responseCache == nil {
		return next
	}
	return h.responseCache.Middleware(next)
}
