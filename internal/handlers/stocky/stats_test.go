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

	mock.ExpectQuery(regexp.QuoteMeta("FROM today_rewards")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"stock_symbol",
			"adjusted_quantity",
		}).AddRow("B", 200.0))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_portfolio")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(10000.0))

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
			"cash_in_lieu_proceeds",
		}).AddRow(1000.0, 25.0, 250.0))

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

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
