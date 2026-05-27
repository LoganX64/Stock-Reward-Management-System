package stocky

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Test that two inserts without idempotency keys pass nil for the idempotency parameter
// and therefore do not collide on a unique empty-string value.
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
