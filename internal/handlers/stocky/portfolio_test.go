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

func TestPortfolioHandler_ReturnsHoldingsAndCashOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", handler.PortfolioHandler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM user_portfolio p")).
		WithArgs(1, 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"adjusted_quantity",
			"current_price",
			"inr_value",
			"price_status",
			"price_as_of",
		}).AddRow("B", 200.0, 50.0, 10000.0, "available", time.Now()))

	mock.ExpectQuery(regexp.QuoteMeta("AND current_price IS NULL")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}))

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"total_portfolio_value",
			"total_positions",
			"valuation_date",
		}).AddRow(10000.0, 1, "2026-10-28"))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
			"cash_in_lieu_proceeds",
		}).AddRow(1000.0, 0.0, 400.0))
	mock.ExpectCommit()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for portfolio request, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["cashProceeds"] != 1000.0 {
		t.Errorf("expected cashProceeds 1000, got %v", body["cashProceeds"])
	}
	if body["cashInLieuProceeds"] != 400.0 {
		t.Errorf("expected cashInLieuProceeds 400, got %v", body["cashInLieuProceeds"])
	}
	if body["writtenOffQuantity"] != 0.0 {
		t.Errorf("expected writtenOffQuantity 0, got %v", body["writtenOffQuantity"])
	}
	if body["valuationComplete"] != true {
		t.Errorf("expected valuationComplete true, got %v", body["valuationComplete"])
	}
	if body["unpricedPositions"] != 0.0 {
		t.Errorf("expected unpricedPositions 0, got %v", body["unpricedPositions"])
	}
	if body["totalPortfolioValue"] != 10000.0 {
		t.Errorf("expected totalPortfolioValue 10000, got %v", body["totalPortfolioValue"])
	}
	if body["totalPositions"] != 1.0 {
		t.Errorf("expected totalPositions 1, got %v", body["totalPositions"])
	}
	if body["valuationDate"] != "2026-10-28" {
		t.Errorf("expected valuationDate 2026-10-28, got %v", body["valuationDate"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestPortfolioHandler_ReturnsMissingPriceHolding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/portfolio/:userId", handler.PortfolioHandler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM user_portfolio p")).
		WithArgs(1, 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"adjusted_quantity",
			"current_price",
			"inr_value",
			"price_status",
			"price_as_of",
		}).
			AddRow("B", 200.0, 50.0, 10000.0, "available", time.Now()).
			AddRow("NEWCO", 10.0, nil, nil, "missing", nil))

	mock.ExpectQuery(regexp.QuoteMeta("AND current_price IS NULL")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}).AddRow("NEWCO"))

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"total_portfolio_value",
			"total_positions",
			"valuation_date",
		}).AddRow(10000.0, 2, "2026-10-28"))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
			"cash_in_lieu_proceeds",
		}).AddRow(0.0, 0.0, 0.0))
	mock.ExpectCommit()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/portfolio/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for portfolio request, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["valuationComplete"] != false {
		t.Errorf("expected valuationComplete false, got %v", body["valuationComplete"])
	}
	if body["unpricedPositions"] != 1.0 {
		t.Errorf("expected unpricedPositions 1, got %v", body["unpricedPositions"])
	}
	if body["totalPortfolioValue"] != 10000.0 {
		t.Errorf("expected priced-only totalPortfolioValue 10000, got %v", body["totalPortfolioValue"])
	}
	if body["totalPositions"] != 2.0 {
		t.Errorf("expected totalPositions to include the unpriced holding, got %v", body["totalPositions"])
	}
	symbols, ok := body["unpricedSymbols"].([]interface{})
	if !ok || len(symbols) != 1 || symbols[0] != "NEWCO" {
		t.Errorf("expected unpricedSymbols [NEWCO], got %v", body["unpricedSymbols"])
	}

	portfolio, ok := body["portfolio"].([]interface{})
	if !ok || len(portfolio) != 2 {
		t.Fatalf("expected two portfolio rows, got %v", body["portfolio"])
	}
	missing, ok := portfolio[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected missing holding object, got %v", portfolio[1])
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

func TestGetPortfolioTotals_ZeroHoldings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*)")).
		WithArgs(9).
		WillReturnRows(sqlmock.NewRows([]string{
			"total_portfolio_value",
			"total_positions",
			"valuation_date",
		}).AddRow(0.0, 0, "2026-10-28"))

	value, positions, valuationDate, err := handler.getPortfolioTotals(db, 9)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !value.IsZero() {
		t.Errorf("expected zero value, got %v", value)
	}
	if positions != 0 {
		t.Errorf("expected zero positions, got %v", positions)
	}
	if valuationDate != "2026-10-28" {
		t.Errorf("expected valuation date 2026-10-28, got %v", valuationDate)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
