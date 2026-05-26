package stocky

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupTodayStocksRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.GET("/api/v1/today-stocks/:userId", handler.GetTodayStocks)

	return router
}

func TestGetTodayStocks_InvalidUserID(t *testing.T) {
	router := setupTodayStocksRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/today-stocks/abc", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid user ID, got %d", w.Code)
	}
}
