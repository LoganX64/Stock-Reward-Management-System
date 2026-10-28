package jobs

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

type fixedPriceFetcher struct {
	price float64
	err   error
}

func (f fixedPriceFetcher) GetLatestPrice(string, float64) (float64, error) {
	return f.price, f.err
}

func TestProcessJob_UnchangedPriceStillWritesHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO stock_price_history")).
		WithArgs("TCS", 100.0).
		WillReturnResult(sqlmock.NewResult(0, 1))

	service := NewPriceService(db, NewPriceCache(), fixedPriceFetcher{price: 100}, nil)
	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: 100,
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestProcessJob_ChangedPriceUpdatesPriceAndWritesHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE stock_prices SET price=$1, updated_at=NOW() WHERE stock_symbol=$2")).
		WithArgs(105.5, "TCS").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO stock_price_history")).
		WithArgs("TCS", 105.5).
		WillReturnResult(sqlmock.NewResult(0, 1))

	service := NewPriceService(db, NewPriceCache(), fixedPriceFetcher{price: 105.5}, nil)
	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: 100,
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}
