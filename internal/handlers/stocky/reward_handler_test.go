package stocky

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// anyTime is a sqlmock argument matcher for time.Time values.
type anyTime struct{}

func (a anyTime) Match(v driver.Value) bool {
	_, ok := v.(time.Time)
	return ok
}

func TestInsertReward_PassesNullIdempotencyWhenEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	req := CreateRewardRequest{
		UserID:         42,
		StockID:        1001,
		RewardDate:     time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC),
		IdempotencyKey: "",
		Quantity:       10,
	}

	// Expect Exec where the 4th arg (idempotency_key) is NULL.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO rewards")).
		WithArgs(req.UserID, req.StockID, anyTime{}, nil, req.Quantity).
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
		StockID:        10,
		RewardDate:     time.Now(),
		IdempotencyKey: "",
		Quantity:       5,
	}
	req2 := CreateRewardRequest{
		UserID:         2,
		StockID:        20,
		RewardDate:     time.Now().Add(24 * time.Hour),
		IdempotencyKey: "",
		Quantity:       7,
	}

	// Expect two execs; both should receive nil for the idempotency param.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO rewards")).
		WithArgs(req1.UserID, req1.StockID, anyTime{}, nil, req1.Quantity).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO rewards")).
		WithArgs(req2.UserID, req2.StockID, anyTime{}, nil, req2.Quantity).
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
