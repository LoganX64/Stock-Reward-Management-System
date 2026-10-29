package stocky

import (
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/shopspring/decimal"
)

func TestGetUserCorporateActionCash_ReturnsOutcome(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(7).
		WillReturnRows(sqlmock.NewRows([]string{
			"cash_proceeds",
			"written_off_quantity",
			"cash_in_lieu_proceeds",
		}).AddRow(1250.5, 12.25, 250.5))

	cash, writtenOff, cashInLieu, err := handler.getUserCorporateActionCash(db, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cash.Equal(decimal.RequireFromString("1250.5")) {
		t.Errorf("expected cash 1250.5, got %v", cash)
	}
	if !writtenOff.Equal(decimal.RequireFromString("12.25")) {
		t.Errorf("expected written-off quantity 12.25, got %v", writtenOff)
	}
	if !cashInLieu.Equal(decimal.RequireFromString("250.5")) {
		t.Errorf("expected cash-in-lieu 250.5, got %v", cashInLieu)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestGetUserCorporateActionCash_NoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	handler := NewHandler(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM user_corporate_action_cash")).
		WithArgs(8).
		WillReturnError(sql.ErrNoRows)

	cash, writtenOff, cashInLieu, err := handler.getUserCorporateActionCash(db, 8)
	if err != nil {
		t.Fatalf("expected no error for missing outcome row, got %v", err)
	}
	if !cash.IsZero() || !writtenOff.IsZero() || !cashInLieu.IsZero() {
		t.Errorf("expected zero values for missing outcome row, got %v/%v/%v", cash, writtenOff, cashInLieu)
	}
}
