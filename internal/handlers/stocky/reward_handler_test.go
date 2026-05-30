package stocky

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

func setupCreateRewardRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(nil)

	router := gin.New()
	Routes(router, handler)

	return router
}

func TestInsertReward_PassesNullIdempotencyWhenEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	req := CreateRewardRequest{
		UserID:         42,
		StockSymbol:    "TCS",
		IdempotencyKey: "",
		Quantity:       10,
	}

	// Expect Exec where the idempotency_key arg is NULL.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO rewards")).
		WithArgs(req.UserID, req.StockSymbol, req.Quantity, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if _, err := InsertReward(context.Background(), db, req); err != nil {
		t.Fatalf("InsertReward returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestInsertReward_TwoInsertsWithoutIdempotencyAllowMultipleRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	req1 := CreateRewardRequest{
		UserID:         1,
		StockSymbol:    "TCS",
		IdempotencyKey: "",
		Quantity:       5,
	}
	req2 := CreateRewardRequest{
		UserID:         2,
		StockSymbol:    "INFY",
		IdempotencyKey: "",
		Quantity:       7,
	}

	// Expect two execs; both should receive nil for the idempotency param.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO rewards")).
		WithArgs(req1.UserID, req1.StockSymbol, req1.Quantity, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO rewards")).
		WithArgs(req2.UserID, req2.StockSymbol, req2.Quantity, nil).
		WillReturnResult(sqlmock.NewResult(2, 1))

	if _, err := InsertReward(context.Background(), db, req1); err != nil {
		t.Fatalf("InsertReward 1 returned error: %v", err)
	}
	if _, err := InsertReward(context.Background(), db, req2); err != nil {
		t.Fatalf("InsertReward 2 returned error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestCreateReward_InvalidJSON(t *testing.T) {
	router := setupCreateRewardRouter()

	payload := `{"user_id":`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/reward", bytes.NewBufferString(payload))
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

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/reward", bytes.NewBufferString(payload))
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

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/reward", bytes.NewBufferString(payload))
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

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/reward", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing stock_symbol, got %d", w.Code)
	}
}

func TestCreateReward_NormalizesStockSymbol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	Routes(router, handler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT price FROM stock_prices WHERE UPPER(stock_symbol) = UPPER($1)")).
		WithArgs("TCS").
		WillReturnRows(sqlmock.NewRows([]string{"price"}).AddRow(100.0))
	mock.ExpectExec("SAVEPOINT reward_insert").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO rewards").
		WithArgs(1, "TCS", 2.0, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"user_id",
			"stock_symbol",
			"quantity",
			"idempotency_key",
			"created_at",
		}).AddRow(1, 1, "TCS", 2.0, "case-test", "2026-05-28T00:00:00Z"))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, "stock_units", "TCS", 2.0, 0.0).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, "inr_outflow", "", 0.0, -200.0).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, "brokerage_fee", "", 0.0, -1.0).
		WillReturnResult(sqlmock.NewResult(3, 1))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, "stt_fee", "", 0.0, -0.2).
		WillReturnResult(sqlmock.NewResult(4, 1))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, "gst_fee", "", 0.0, -0.216).
		WillReturnResult(sqlmock.NewResult(5, 1))
	mock.ExpectCommit()

	payload := `{
		"user_id": 1,
		"stock_symbol": " tcs ",
		"quantity": 2,
		"idempotency_key": "case-test"
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/reward", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for normalized stock symbol, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestCreateReward_IdempotentRetryWinsOverDateConstraint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	Routes(router, handler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT price FROM stock_prices WHERE UPPER(stock_symbol) = UPPER($1)")).
		WithArgs("TCS").
		WillReturnRows(sqlmock.NewRows([]string{"price"}).AddRow(100.0))
	mock.ExpectExec("SAVEPOINT reward_insert").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO rewards").
		WithArgs(1, "TCS", 2.0, sqlmock.AnyArg()).
		WillReturnError(&pq.Error{
			Code:       "23505",
			Constraint: "unique_user_stock_date",
		})
	mock.ExpectExec("ROLLBACK TO SAVEPOINT reward_insert").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, user_id, stock_symbol, quantity, COALESCE(idempotency_key, ''), created_at")).
		WithArgs("retry-key").
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"user_id",
			"stock_symbol",
			"quantity",
			"idempotency_key",
			"created_at",
		}).AddRow(1, 1, "TCS", 2.0, "retry-key", "2026-05-28T00:00:00Z"))
	mock.ExpectCommit()

	payload := `{
		"user_id": 1,
		"stock_symbol": "TCS",
		"quantity": 2,
		"idempotency_key": "retry-key"
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/reward", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for idempotent retry, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
