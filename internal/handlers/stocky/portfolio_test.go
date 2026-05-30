package stocky

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupPortfolioRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", handler.PortfolioHandler)

	return router
}

func TestPortfolioHandler_InvalidUserID(t *testing.T) {
	router := setupPortfolioRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/portfolio/abc", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid user ID, got %d", w.Code)
	}
}

func TestPortfolioHandler_InvalidOffset(t *testing.T) {
	router := setupPortfolioRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/portfolio/1?offset=-1", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid offset, got %d", w.Code)
	}
}
