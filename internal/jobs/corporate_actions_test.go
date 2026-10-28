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

	mock.ExpectQuery(regexp.QuoteMeta("process_corporate_action_cash(CURRENT_DATE)")).
		WillReturnRows(sqlmock.NewRows([]string{"process_corporate_action_cash"}).AddRow(3))

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

	mock.ExpectQuery(regexp.QuoteMeta("process_corporate_action_cash(CURRENT_DATE)")).
		WillReturnError(context.DeadlineExceeded)

	service := NewCorporateActionService(db)
	service.process(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
