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

func TestGetHistoricalINR_InvalidAsOf(t *testing.T) {
	router := setupHistoricalRouter()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical-inr/1?asOf=10-01-2026", nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid asOf, got %d", w.Code)
	}
}

func TestGetHistoricalINR_RejectsFutureAsOf(t *testing.T) {
	router := setupHistoricalRouter()

	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical-inr/1?asOf="+tomorrow, nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for future asOf, got %d", w.Code)
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

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM historical_rewards_as_of")).
		WithArgs(1, nil, 100, 0).
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
			"outcome_type",
			"is_active",
			"delist_effective_date",
			"cash_proceeds",
			"written_off_quantity",
		}).
			AddRow("2026-10-27", 2, "TCS", 10.0, 3500.0, 0.0, 35000.0, "available", time.Now(), "active", true, nil, 0.0, 0.0).
			AddRow("2026-10-26", 1, "NEWCO", 5.0, nil, 0.0, nil, "missing", nil, "active", true, nil, 0.0, 0.0))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT CURRENT_DATE::TEXT")).
		WillReturnRows(sqlmock.NewRows([]string{"current_date"}).AddRow("2026-10-28"))

	mock.ExpectQuery(regexp.QuoteMeta("AND price_status = 'missing'")).
		WithArgs(1, nil).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}).AddRow("NEWCO"))
	mock.ExpectCommit()

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
	if body["valuationMode"] != "grant_date" {
		t.Errorf("expected valuationMode grant_date, got %v", body["valuationMode"])
	}
	if body["valuationDate"] != "2026-10-28" {
		t.Errorf("expected valuationDate 2026-10-28, got %v", body["valuationDate"])
	}
	if body["asOf"] != nil {
		t.Errorf("expected asOf null, got %v", body["asOf"])
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
	if available["outcomeType"] != "active" {
		t.Errorf("expected outcomeType active, got %v", available["outcomeType"])
	}
	if available["isActive"] != true {
		t.Errorf("expected isActive true, got %v", available["isActive"])
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

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestGetHistoricalINR_AsOfKeepsDelistedReward(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.GET("/api/v1/historical-inr/:userId", handler.GetHistoricalINR)

	asOf := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM historical_rewards_as_of")).
		WithArgs(1, asOf, 100, 0).
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
			"outcome_type",
			"is_active",
			"delist_effective_date",
			"cash_proceeds",
			"written_off_quantity",
		}).
			AddRow("2026-09-15", 5, "OLDCO", 0.0, nil, 0.0, 0.0, "not_required", nil, "write_off", false, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 0.0, 100.0).
			AddRow("2026-09-10", 6, "CASHCO", 0.0, nil, 0.0, 0.0, "not_required", nil, "cash_exit", false, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), 5000.0, 0.0))

	mock.ExpectQuery(regexp.QuoteMeta("AND price_status = 'missing'")).
		WithArgs(1, asOf).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol"}))
	mock.ExpectCommit()

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/historical-inr/1?asOf="+asOf, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for as-of historical request, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["valuationMode"] != "as_of" {
		t.Errorf("expected valuationMode as_of, got %v", body["valuationMode"])
	}
	if body["valuationDate"] != asOf {
		t.Errorf("expected valuationDate %s, got %v", asOf, body["valuationDate"])
	}
	if body["asOf"] != asOf {
		t.Errorf("expected asOf %s, got %v", asOf, body["asOf"])
	}
	// A zero-quantity terminal outcome needs no price. It is not a valuation
	// gap, so valuationComplete remains true.
	if body["valuationComplete"] != true {
		t.Errorf("expected valuationComplete true, got %v", body["valuationComplete"])
	}
	if body["unpricedRewards"] != 0.0 {
		t.Errorf("expected unpricedRewards 0, got %v", body["unpricedRewards"])
	}

	history, ok := body["history"].([]interface{})
	if !ok || len(history) != 2 {
		t.Fatalf("expected two historical rows, got %v", body["history"])
	}

	writeOff, ok := history[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected write-off object, got %v", history[0])
	}
	if writeOff["outcomeType"] != "write_off" {
		t.Errorf("expected outcomeType write_off, got %v", writeOff["outcomeType"])
	}
	if writeOff["isActive"] != false {
		t.Errorf("expected isActive false, got %v", writeOff["isActive"])
	}
	if writeOff["delistEffectiveDate"] != "2026-09-20" {
		t.Errorf("expected delistEffectiveDate 2026-09-20, got %v", writeOff["delistEffectiveDate"])
	}
	if writeOff["priceStatus"] != "not_required" {
		t.Errorf("expected priceStatus not_required, got %v", writeOff["priceStatus"])
	}
	if writeOff["price"] != nil {
		t.Errorf("expected price null, got %v", writeOff["price"])
	}
	if writeOff["inrValue"] != 0.0 {
		t.Errorf("expected inrValue 0, got %v", writeOff["inrValue"])
	}
	if writeOff["writtenOffQuantity"] != 100.0 {
		t.Errorf("expected writtenOffQuantity 100, got %v", writeOff["writtenOffQuantity"])
	}

	cashExit, ok := history[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected cash-exit object, got %v", history[1])
	}
	if cashExit["outcomeType"] != "cash_exit" {
		t.Errorf("expected outcomeType cash_exit, got %v", cashExit["outcomeType"])
	}
	if cashExit["cashProceeds"] != 5000.0 {
		t.Errorf("expected cashProceeds 5000, got %v", cashExit["cashProceeds"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
