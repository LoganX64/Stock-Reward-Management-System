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
