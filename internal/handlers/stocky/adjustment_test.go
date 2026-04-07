package stocky

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.POST("/api/v1/adjustments/:id", handler.adjustmentHandler)

	return router
}

func TestAdjustmentHandler_InvalidRewardID(t *testing.T) {
	router := setupRouter()

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/adjustments/abc", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid reward ID, got %d", w.Code)
	}
}

func TestAdjustmentHandler_InvalidJSON(t *testing.T) {
	router := setupRouter()

	payload := `{"adjustment_type":`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/adjustments/1", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestAdjustmentHandler_InvalidAdjustmentType(t *testing.T) {
	router := setupRouter()

	payload := `{
		"adjustment_type": "invalid_type",
		"delta_quantity": 10,
		"delta_amount": 100
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/adjustments/1", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid adjustment type, got %d", w.Code)
	}
}
