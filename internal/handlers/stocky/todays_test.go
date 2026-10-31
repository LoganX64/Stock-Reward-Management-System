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

func TestGetTodayStocks_ReturnsMissingPriceReward(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/today-stocks/:userId", handler.GetTodayStocks)

	mock.ExpectQuery(regexp.QuoteMeta("FROM today_rewards")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"reward_event_id",
			"stock_symbol",
			"adjusted_quantity",
			"current_price",
			"total_adjustment_amount",
			"inr_value",
			"price_status",
			"price_as_of",
			"price_source",
			"price_fetched_at",
		}).
			AddRow(1, "TCS", 10.0, 3500.0, 0.0, 35000.0, "available", time.Now(), "yahoo", time.Now()).
			AddRow(2, "NEWCO", 5.0, nil, 0.0, nil, "missing", nil, nil, nil))

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/today-stocks/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for today stocks request, got %d", w.Code)
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

	stocks, ok := body["stocks"].([]interface{})
	if !ok || len(stocks) != 2 {
		t.Fatalf("expected two stock rows, got %v", body["stocks"])
	}
	missing, ok := stocks[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected missing stock object, got %v", stocks[1])
	}
	if missing["priceStatus"] != "missing" {
		t.Errorf("expected priceStatus missing, got %v", missing["priceStatus"])
	}
	if missing["currentPrice"] != nil {
		t.Errorf("expected currentPrice null, got %v", missing["currentPrice"])
	}
	if missing["inrValue"] != nil {
		t.Errorf("expected inrValue null, got %v", missing["inrValue"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
