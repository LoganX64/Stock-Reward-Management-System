package stocky

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupStatsRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.GET("/api/v1/stats/:userId", handler.StatsHandler)

	return router
}

func TestStatsHandler_InvalidUserID(t *testing.T) {
	router := setupStatsRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/stats/abc", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid user ID, got %d", w.Code)
	}
}
