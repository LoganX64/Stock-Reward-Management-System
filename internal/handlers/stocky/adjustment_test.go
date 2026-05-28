package stocky

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/LoganX64/stocky-api/internal/storage/models"
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

func TestAdjustmentHandler_RewardReversalUsesPositiveRefundAmount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)
	router := gin.New()
	router.POST("/api/v1/adjustments/:id", handler.adjustmentHandler)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT quantity, stock_symbol FROM rewards WHERE id = $1 FOR UPDATE")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"quantity", "stock_symbol"}).AddRow(5.0, "TCS"))
	mock.ExpectQuery("INSERT INTO adjustments").
		WithArgs(1, models.Reward_Reversal, -1.0, 100.0, "reversal refund").
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"reward_id",
			"adjustment_type",
			"delta_quantity",
			"delta_amount",
			"reason",
			"created_at",
		}).AddRow(1, 1, models.Reward_Reversal, -1.0, 100.0, "reversal refund", "2026-05-28T00:00:00Z"))
	mock.ExpectExec("UPDATE rewards").
		WithArgs(-1.0, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, models.StockUnits, "TCS", -1.0, 0.0).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO ledger").
		WithArgs(1, models.INROutflow, "", 0.0, 100.0).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	payload := `{
		"adjustment_type": "reward_reversal",
		"delta_quantity": -1,
		"delta_amount": 100,
		"reason": "reversal refund"
	}`

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/adjustments/1", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for reward reversal, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
