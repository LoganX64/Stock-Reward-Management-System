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

var freshnessTestNow = time.Date(2026, time.November, 2, 11, 30, 0, 0, time.FixedZone("IST", 5*3600+1800))

func TestGetTodayStocks_ReportsFreshnessAndProvenance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	now := freshnessTestNow
	handler := NewHandler(db, WithClock(func() time.Time { return now }))
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
			AddRow(1, "TCS", 10.0, 100.0, 0.0, 1000.0, "available", now.Add(-30*time.Minute), "yahoo", now.Add(-29*time.Minute)).
			AddRow(2, "OLD", 5.0, 90.0, 0.0, 450.0, "available", now.Add(-2*time.Hour), "yahoo", now.Add(-119*time.Minute)).
			AddRow(3, "LEG", 2.0, 80.0, 0.0, 160.0, "available", now.Add(-30*time.Minute), "legacy", now.Add(-29*time.Minute)).
			AddRow(4, "MISS", 1.0, nil, 0.0, nil, "missing", nil, nil, nil))

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/today-stocks/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["staleRewards"] != 1.0 {
		t.Errorf("expected staleRewards 1, got %v", body["staleRewards"])
	}
	symbols, ok := body["staleSymbols"].([]interface{})
	if !ok || len(symbols) != 1 || symbols[0] != "OLD" {
		t.Errorf("expected staleSymbols [OLD], got %v", body["staleSymbols"])
	}
	if body["pricesFresh"] != false {
		t.Errorf("expected pricesFresh false, got %v", body["pricesFresh"])
	}
	if body["valuationComplete"] != false {
		t.Errorf("expected valuationComplete false, got %v", body["valuationComplete"])
	}

	stocks, ok := body["stocks"].([]interface{})
	if !ok || len(stocks) != 4 {
		t.Fatalf("expected four stock rows, got %v", body["stocks"])
	}

	fresh := stocks[0].(map[string]interface{})
	if fresh["priceFreshness"] != "fresh" {
		t.Errorf("expected fresh priceFreshness, got %v", fresh["priceFreshness"])
	}
	if fresh["priceSource"] != "yahoo" {
		t.Errorf("expected priceSource yahoo, got %v", fresh["priceSource"])
	}
	wantFetched := now.Add(-29 * time.Minute).UTC().Format(time.RFC3339)
	if fresh["priceFetchedAt"] != wantFetched {
		t.Errorf("expected priceFetchedAt %s, got %v", wantFetched, fresh["priceFetchedAt"])
	}

	stale := stocks[1].(map[string]interface{})
	if stale["priceFreshness"] != "stale" {
		t.Errorf("expected stale priceFreshness, got %v", stale["priceFreshness"])
	}

	legacy := stocks[2].(map[string]interface{})
	if legacy["priceFreshness"] != "unknown" {
		t.Errorf("expected unknown priceFreshness for legacy source, got %v", legacy["priceFreshness"])
	}

	missing := stocks[3].(map[string]interface{})
	if missing["priceFreshness"] != nil {
		t.Errorf("expected null priceFreshness for missing price, got %v", missing["priceFreshness"])
	}
	if missing["priceSource"] != nil {
		t.Errorf("expected null priceSource for missing price, got %v", missing["priceSource"])
	}
	if missing["priceFetchedAt"] != nil {
		t.Errorf("expected null priceFetchedAt for missing price, got %v", missing["priceFetchedAt"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestGetTodayStocks_MissingPricesAreNotStale(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	now := freshnessTestNow
	handler := NewHandler(db, WithClock(func() time.Time { return now }))
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
		}).AddRow(1, "MISS", 1.0, nil, 0.0, nil, "missing", nil, nil, nil))

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/today-stocks/1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["staleRewards"] != 0.0 {
		t.Errorf("expected staleRewards 0, got %v", body["staleRewards"])
	}
	if body["pricesFresh"] != true {
		t.Errorf("expected pricesFresh true when the only price is missing, got %v", body["pricesFresh"])
	}
}

func TestPortfolioHandler_FullSetFreshnessIgnoresPagination(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	now := freshnessTestNow
	handler := NewHandler(db, WithClock(func() time.Time { return now }))
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
			"price_source",
			"price_fetched_at",
		}).AddRow("A", 10.0, 100.0, 1000.0, "available", now.Add(-30*time.Minute), "yahoo", now.Add(-29*time.Minute)))

	mock.ExpectQuery(regexp.QuoteMeta("AND current_price IS NULL")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}))

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"total_portfolio_value",
			"total_positions",
			"valuation_date",
		}).AddRow(1000.0, 2, "2026-11-02"))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT stock_symbol, price_status, price_as_of, price_source")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"price_status",
			"price_as_of",
			"price_source",
		}).
			AddRow("A", "available", now.Add(-30*time.Minute), "yahoo").
			AddRow("Z", "available", now.Add(-2*time.Hour), "yahoo"))

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
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["stalePositions"] != 1.0 {
		t.Errorf("expected stalePositions 1 from the full set, got %v", body["stalePositions"])
	}
	symbols, ok := body["staleSymbols"].([]interface{})
	if !ok || len(symbols) != 1 || symbols[0] != "Z" {
		t.Errorf("expected staleSymbols [Z], got %v", body["staleSymbols"])
	}
	if body["pricesFresh"] != false {
		t.Errorf("expected pricesFresh false, got %v", body["pricesFresh"])
	}

	portfolio, ok := body["portfolio"].([]interface{})
	if !ok || len(portfolio) != 1 {
		t.Fatalf("expected one page row, got %v", body["portfolio"])
	}
	row := portfolio[0].(map[string]interface{})
	if row["priceFreshness"] != "fresh" {
		t.Errorf("expected page row freshness fresh, got %v", row["priceFreshness"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
