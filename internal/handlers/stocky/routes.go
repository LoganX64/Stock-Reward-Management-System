package stocky

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"

	"net/http"
	"strconv"

	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"

	"github.com/sirupsen/logrus"
)

var db *sql.DB

// InitDB initializes the global database connection.
func InitDB(database *sql.DB) {
	db = database
}

// generateRequestID creates a short, collision-resistant request ID
func generateRequestID() string {
	b := make([]byte, 8) // 16 hex characters
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// RequestIDLogger middleware adds a unique request_id to context and logger
func RequestIDLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := generateRequestID()
		c.Set("request_id", reqID)

		entry := logrus.WithField("request_id", reqID)
		c.Set("logger", entry)

		c.Next()
	}
}

// requestID returns the request ID from context (safe fallback)
func requestID(c *gin.Context) string {
	if id, exists := c.Get("request_id"); exists {
		if str, ok := id.(string); ok {
			return str
		}
	}
	return "unknown"
}

// getLogger returns a logger with request_id already attached (recommended)
func getLogger(c *gin.Context) *logrus.Entry {
	if logger, exists := c.Get("logger"); exists {
		if entry, ok := logger.(*logrus.Entry); ok {
			return entry
		}
	}
	return logrus.WithField("request_id", requestID(c))
}

// parseUserID extracts and validates userId from URL param
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

// parseRewardID extracts and validates reward ID (used in adjustment route)
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

func Routes(r *gin.Engine) {
	// Global middleware
	r.Use(RequestIDLogger())

	// Health Check Endpoint
	r.GET("/health", func(c *gin.Context) {
		logrus.WithField("request_id", requestID(c)).Info("Health check endpoint hit")
		response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
			"status": "OK",
		})
	})

	// API v1 routes group
	v1 := r.Group("/api/v1")
	{
		v1.POST("/reward", CreateReward)
		// User-specific routes
		v1.GET("/today-stocks/:userId", GetTodayStocks)
		v1.GET("/historical-inr/:userId", GetHistoricalINR)
		v1.GET("/stats/:userId", StatsHandler)
		v1.GET("/portfolio/:userId", PortfolioHandler)
		// Adjustment route
		v1.POST("/adjustments/:id", adjustmentHandler)
	}

}
