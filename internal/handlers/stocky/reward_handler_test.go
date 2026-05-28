package stocky

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

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
