package jobs

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCorporateActionService_Process(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("process_corporate_action_cash_if_due(market_today())")).
		WillReturnRows(sqlmock.NewRows([]string{"process_corporate_action_cash_if_due"}).AddRow(3))
	mock.ExpectQuery(regexp.QuoteMeta("refresh_instrument_factors_if_due(market_today())")).
		WillReturnRows(sqlmock.NewRows([]string{"refresh_instrument_factors_if_due"}).AddRow(3000))
	mock.ExpectQuery(regexp.QuoteMeta("refresh_effective_corporate_actions_if_due(market_today())")).
		WillReturnRows(sqlmock.NewRows([]string{"refresh_effective_corporate_actions_if_due"}).AddRow(12))

	service := NewCorporateActionService(db)
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestCorporateActionService_ProcessErrorDoesNotPanic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("process_corporate_action_cash_if_due(market_today())")).
		WillReturnError(context.DeadlineExceeded)
	mock.ExpectQuery(regexp.QuoteMeta("refresh_instrument_factors_if_due(market_today())")).
		WillReturnRows(sqlmock.NewRows([]string{"refresh_instrument_factors_if_due"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta("refresh_effective_corporate_actions_if_due(market_today())")).
		WillReturnRows(sqlmock.NewRows([]string{"refresh_effective_corporate_actions_if_due"}).AddRow(0))

	service := NewCorporateActionService(db)
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
