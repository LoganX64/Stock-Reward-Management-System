package stocky

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func setupHistoricalRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	router.GET("/api/v1/historical-inr/:userId", handler.GetHistoricalINR)

	return router
}

func TestGetHistoricalINR_InvalidUserID(t *testing.T) {
	router := setupHistoricalRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical-inr/abc", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid user ID, got %d", w.Code)
	}
}

func TestGetHistoricalINR_InvalidLimit(t *testing.T) {
	router := setupHistoricalRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical-inr/1?limit=zero", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid limit, got %d", w.Code)
	}
}

func TestGetHistoricalINR_ReturnsMissingHistoricalPrice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/historical-inr/:userId", handler.GetHistoricalINR)

	mock.ExpectQuery(regexp.QuoteMeta("FROM historical_rewards")).
		WithArgs(1, 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"reward_date",
			"reward_event_id",
			"stock_symbol",
			"adjusted_quantity",
			"price",
			"total_adjustment_amount",
			"inr_value",
			"price_status",
			"price_as_of",
		}).
			AddRow("2026-10-27", 2, "TCS", 10.0, 3500.0, 0.0, 35000.0, "available", time.Now()).
			AddRow("2026-10-26", 1, "NEWCO", 5.0, nil, 0.0, nil, "missing", nil))

	mock.ExpectQuery(regexp.QuoteMeta("AND price IS NULL")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}).AddRow("NEWCO"))

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical-inr/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for historical INR request, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["valuationComplete"] != false {
		t.Errorf("expected valuationComplete false, got %v", body["valuationComplete"])
	}
	if body["unpricedRewards"] != 1.0 {
		t.Errorf("expected unpricedRewards 1, got %v", body["unpricedRewards"])
	}
	symbols, ok := body["unpricedSymbols"].([]interface{})
	if !ok || len(symbols) != 1 || symbols[0] != "NEWCO" {
		t.Errorf("expected unpricedSymbols [NEWCO], got %v", body["unpricedSymbols"])
	}

	history, ok := body["history"].([]interface{})
	if !ok || len(history) != 2 {
		t.Fatalf("expected two historical rows, got %v", body["history"])
	}

	missing, ok := history[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected missing historical object, got %v", history[1])
	}
	if missing["stockSymbol"] != "NEWCO" {
		t.Errorf("expected NEWCO missing row, got %v", missing["stockSymbol"])
	}
	if missing["priceStatus"] != "missing" {
		t.Errorf("expected priceStatus missing, got %v", missing["priceStatus"])
	}
	if missing["price"] != nil {
		t.Errorf("expected price null, got %v", missing["price"])
	}
	if missing["inrValue"] != nil {
		t.Errorf("expected inrValue null, got %v", missing["inrValue"])
	}
	if missing["priceAsOf"] != nil {
		t.Errorf("expected priceAsOf null, got %v", missing["priceAsOf"])
	}

	available, ok := history[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected available historical object, got %v", history[0])
	}
	if available["priceStatus"] != "available" {
		t.Errorf("expected priceStatus available, got %v", available["priceStatus"])
	}
	if available["price"] != 3500.0 {
		t.Errorf("expected price 3500, got %v", available["price"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
