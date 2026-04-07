package stocky

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupHistoricalRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.GET("/api/v1/historical/:user_id", handler.GetHistoricalINR)

	return router
}

func TestGetHistoricalINR_InvalidUserID(t *testing.T) {
	router := setupHistoricalRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical/abc", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid user ID, got %d", w.Code)
	}
}
