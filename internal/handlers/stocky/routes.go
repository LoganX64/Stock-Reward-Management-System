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
			response.ErrorResponse("invalid userId – must be a positive integer"))
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
			response.ErrorResponse("invalid reward ID – must be a positive integer"))
		return 0, false
	}
	return id, true
}

// Routes now accepts the handler
func Routes(r *gin.Engine, handler *Handler) {
	r.Use(RequestIDLogger())

	// Health check
	r.GET("/health", func(c *gin.Context) {
		getLogger(c).Info("Health check endpoint hit")
		response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
			"status": "OK",
		})
	})

	// API v1 group
	v1 := r.Group("/api/v1")
	{
		v1.POST("/reward", handler.CreateReward)
		v1.POST("/adjustments/:id", handler.adjustmentHandler)

		v1.GET("/today-stocks/:userId", handler.GetTodayStocks)
		v1.GET("/historical-inr/:userId", handler.GetHistoricalINR)
		v1.GET("/stats/:userId", handler.StatsHandler)
		v1.GET("/portfolio/:userId", handler.PortfolioHandler)
	}
}
