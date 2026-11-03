package stocky

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestReadHandlerUsesReadReplica(t *testing.T) {
	gin.SetMode(gin.TestMode)

	primary, primaryMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("primary sqlmock: %v", err)
	}
	defer primary.Close()

	replica, replicaMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("replica sqlmock: %v", err)
	}
	defer replica.Close()

	replicaMock.ExpectQuery("FROM reward_state rs").
		WithArgs(7).
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
		}))

	handler := NewHandler(primary, WithReadDB(replica))
	router := gin.New()
	router.GET("/api/v1/today-stocks/:userId", handler.GetTodayStocks)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/today-stocks/7", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := replicaMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("read replica expectations: %v", err)
	}
	if err := primaryMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("primary should not serve reads: %v", err)
	}
}

func TestWriteHandlerUsesPrimaryPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	primary, primaryMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("primary sqlmock: %v", err)
	}
	defer primary.Close()

	replica, replicaMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("replica sqlmock: %v", err)
	}
	defer replica.Close()

	primaryMock.ExpectBegin().WillReturnError(errors.New("primary unavailable"))

	handler := NewHandler(primary, WithReadDB(replica))
	router := gin.New()
	router.POST("/api/v1/reward", handler.CreateReward)

	body := strings.NewReader(`{"user_id":1,"stock_symbol":"TCS","quantity":10}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reward", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if err := primaryMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("primary expectations: %v", err)
	}
	if err := replicaMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("write path must not use the read replica: %v", err)
	}
}
