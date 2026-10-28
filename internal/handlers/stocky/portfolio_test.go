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

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_portfolio p")).
		WithArgs(1, 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"adjusted_quantity",
			"current_price",
			"inr_value",
		}).AddRow("B", 200.0, 50.0, 10000.0))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
		}).AddRow(1000.0, 0.0))

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
	if body["writtenOffQuantity"] != 0.0 {
		t.Errorf("expected writtenOffQuantity 0, got %v", body["writtenOffQuantity"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
