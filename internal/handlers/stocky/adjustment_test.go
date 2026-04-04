package stocky

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAdjustmentHandler_InvalidType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Use NewHandler with nil DB - safe for validation tests
	handler := NewHandler(nil)

	router := gin.New()
	router.POST("/api/v1/adjustments/:id", handler.adjustmentHandler)

	payload := `{
		"adjustment_type": "invalid_type",
		"delta_quantity": 10
	}`

	req, err := http.NewRequest(http.MethodPost, "/api/v1/adjustments/1", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid adjustment type, got %d", w.Code)
	}
}

func TestAdjustmentHandler_MissingPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.POST("/api/v1/adjustments/:id", handler.adjustmentHandler)

	req, err := http.NewRequest(http.MethodPost, "/api/v1/adjustments/1", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid JSON payload, got %d", w.Code)
	}
}

func TestAdjustmentHandler_InvalidRewardID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.POST("/api/v1/adjustments/:id", handler.adjustmentHandler)

	req, err := http.NewRequest(http.MethodPost, "/api/v1/adjustments/abc", nil)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid reward ID, got %d", w.Code)
	}
}

func TestAdjustmentHandler_NegativeQuantity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// For negative quantity test, we still need a real DB because it does a query
	// For now, we'll skip full DB test or use a test DB later
	// This test will likely fail until we have a proper test database
	t.Skip("Requires test database - will implement after setting up test DB")
}
