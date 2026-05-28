package stocky

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupCreateRewardRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.POST("/api/v1/rewards", handler.CreateReward)

	return router
}

func TestCreateReward_InvalidJSON(t *testing.T) {
	router := setupCreateRewardRouter()

	payload := `{"user_id":`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/rewards", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestCreateReward_ZeroQuantity(t *testing.T) {
	router := setupCreateRewardRouter()

	payload := `{
		"user_id": 1,
		"stock_symbol": "AAPL",
		"quantity": 0
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/rewards", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for zero quantity, got %d", w.Code)
	}
}

func TestCreateReward_NegativeQuantity(t *testing.T) {
	router := setupCreateRewardRouter()

	payload := `{
		"user_id": 1,
		"stock_symbol": "AAPL",
		"quantity": -5
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/rewards", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for negative quantity, got %d", w.Code)
	}
}

func TestCreateReward_MissingStockSymbol(t *testing.T) {
	router := setupCreateRewardRouter()

	payload := `{
		"user_id": 1,
		"quantity": 10
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/rewards", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing stock_symbol, got %d", w.Code)
	}
}
