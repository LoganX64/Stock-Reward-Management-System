package stocky

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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

func TestStatsHandler_ReturnsPortfolioAndCashOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/stats/:userId", handler.StatsHandler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM today_rewards")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"adjusted_quantity",
		}).AddRow("B", 200.0))

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"total_portfolio_value",
			"total_positions",
			"valuation_date",
		}).AddRow(10000.0, 1, "2026-10-28"))

	mock.ExpectQuery(regexp.QuoteMeta("AND current_price IS NULL")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
			"cash_in_lieu_proceeds",
		}).AddRow(1000.0, 25.0, 250.0))
	mock.ExpectCommit()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/stats/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for stats request, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["totalPortfolioValue"] != 10000.0 {
		t.Errorf("expected totalPortfolioValue 10000, got %v", body["totalPortfolioValue"])
	}
	if body["cashProceeds"] != 1000.0 {
		t.Errorf("expected cashProceeds 1000, got %v", body["cashProceeds"])
	}
	if body["cashInLieuProceeds"] != 250.0 {
		t.Errorf("expected cashInLieuProceeds 250, got %v", body["cashInLieuProceeds"])
	}
	if body["writtenOffQuantity"] != 25.0 {
		t.Errorf("expected writtenOffQuantity 25, got %v", body["writtenOffQuantity"])
	}
	if body["valuationComplete"] != true {
		t.Errorf("expected valuationComplete true, got %v", body["valuationComplete"])
	}
	if body["unpricedPositions"] != 0.0 {
		t.Errorf("expected unpricedPositions 0, got %v", body["unpricedPositions"])
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

func TestStatsHandler_MarksIncompleteValuationForMissingPrice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/stats/:userId", handler.StatsHandler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM today_rewards")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"adjusted_quantity",
		}).AddRow("NEWCO", 10.0))

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"total_portfolio_value",
			"total_positions",
			"valuation_date",
		}).AddRow(5000.0, 2, "2026-10-28"))

	mock.ExpectQuery(regexp.QuoteMeta("AND current_price IS NULL")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}).AddRow("NEWCO"))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
			"cash_in_lieu_proceeds",
		}).AddRow(0.0, 0.0, 0.0))
	mock.ExpectCommit()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/stats/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for stats request, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["totalPortfolioValue"] != 5000.0 {
		t.Errorf("expected priced-only totalPortfolioValue 5000, got %v", body["totalPortfolioValue"])
	}
	if body["valuationComplete"] != false {
		t.Errorf("expected valuationComplete false, got %v", body["valuationComplete"])
	}
	if body["unpricedPositions"] != 1.0 {
		t.Errorf("expected unpricedPositions 1, got %v", body["unpricedPositions"])
	}
	if body["totalPositions"] != 2.0 {
		t.Errorf("expected totalPositions to include the unpriced holding, got %v", body["totalPositions"])
	}
	symbols, ok := body["unpricedSymbols"].([]interface{})
	if !ok || len(symbols) != 1 || symbols[0] != "NEWCO" {
		t.Errorf("expected unpricedSymbols [NEWCO], got %v", body["unpricedSymbols"])
	}

	rewards, ok := body["todayRewards"].([]interface{})
	if !ok || len(rewards) != 1 {
		t.Fatalf("expected missing-price reward to remain in todayRewards, got %v", body["todayRewards"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
