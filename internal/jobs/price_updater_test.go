package jobs

import (
	"context"
	"regexp"
	"testing"
	"time"

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

func TestProcessJob_NormalizesSymbolBeforeWrites(t *testing.T) {
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
		Symbol:   " tcs ",
		OldPrice: 100,
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("there were unfulfilled expectations: %v", err)
	}
}

func TestQuerySymbolPrices_NormalizesSymbols(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock database: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT stock_symbol, price, updated_at FROM stock_prices")).
		WillReturnRows(sqlmock.NewRows([]string{"stock_symbol", "price", "updated_at"}).
			AddRow(" tcs ", 100.0, time.Now()))

	service := NewPriceService(db, NewPriceCache(), fixedPriceFetcher{}, nil)
	entries, err := service.querySymbolPrices(context.Background())
	if err != nil {
		t.Fatalf("querySymbolPrices returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("querySymbolPrices returned %d entries, want 1", len(entries))
	}
	if entries[0].Symbol != "TCS" {
		t.Errorf("querySymbolPrices symbol = %q, want TCS", entries[0].Symbol)
	}
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
