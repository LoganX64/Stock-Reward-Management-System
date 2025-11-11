package stocky

import (
	"crypto/rand"
	"database/sql"

	"fmt"

	"net/http"
	"strconv"

	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"

	"github.com/sirupsen/logrus"
)

var db *sql.DB

func InitDB(database *sql.DB) {
	db = database
}
func shortRequestID() string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%08x", b)
}

func RequestIDLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("request_id", shortRequestID())
		c.Next()
	}
}
func requestID(c *gin.Context) string {
	id, _ := c.Get("request_id")
	return id.(string)
}

func parseUserID(c *gin.Context) (int, bool) {
	idStr := c.Param("userId")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("invalid userId – must be a positive integer"))
		return 0, false
	}
	return id, true
}

func Routes(r *gin.Engine) {
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
		v1.GET("/today-stocks/:userId", GetTodayStocks)
		v1.GET("/historical-inr/:userId", GetHistoricalINR)
		v1.GET("/stats/:userId", StatsHandler)
		v1.GET("/portfolio/:userId", PortfolioHandler)
		v1.POST("/adjustments/:id", adjustmentHandler)
	}

}
