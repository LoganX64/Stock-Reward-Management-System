package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/marketdata"
)

var priceStoreTestNow = time.Date(2026, 10, 30, 10, 5, 0, 0, time.UTC)

func priceStoreTestQuote() marketdata.Quote {
	return marketdata.Quote{
		Symbol:         "TCS",
		ProviderSymbol: "TCS.NS",
		Price:          decimal.RequireFromString("3500.2500"),
		Currency:       marketdata.CurrencyINR,
		Exchange:       marketdata.ExchangeNSE,
		AsOf:           time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
		FetchedAt:      time.Date(2026, 10, 30, 10, 0, 30, 0, time.UTC),
		Source:         marketdata.SourceYahoo,
	}
}

func newPriceStoreMock(t *testing.T) (*SQLPriceStore, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("open sqlmock database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := NewPriceStore(db, &PriceStoreOptions{
		Now: func() time.Time { return priceStoreTestNow },
	})
	return store, mock
}

func expectQuoteWrites(
	mock sqlmock.Sqlmock,
	quote marketdata.Quote,
	currentRows, historyRows int64,
) {
	mock.ExpectBegin()
	mock.ExpectExec(upsertCurrentPriceSQL).
		WithArgs(
			quote.Symbol,
			quote.Price,
			quote.AsOf,
			quote.Source,
			quote.FetchedAt,
			quote.ProviderSymbol,
			quote.Exchange,
		).
		WillReturnResult(sqlmock.NewResult(0, currentRows))
	mock.ExpectExec(upsertHistorySQL).
		WithArgs(
			quote.Symbol,
			quote.Price,
			quote.AsOf,
			quote.AsOf,
			quote.Source,
			quote.FetchedAt,
			quote.ProviderSymbol,
			quote.Exchange,
		).
		WillReturnResult(sqlmock.NewResult(0, historyRows))
}

func TestSaveQuoteWritesCurrentAndHistoryInOneTransaction(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()

	expectQuoteWrites(mock, quote, 1, 1)
	mock.ExpectCommit()

	result, err := store.SaveQuote(context.Background(), quote)
	if err != nil {
		t.Fatalf("SaveQuote returned error: %v", err)
	}
	if !result.CurrentAccepted || !result.HistoryChanged {
		t.Errorf("SaveQuote result = %+v, want both writes accepted", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

func TestSaveQuoteReportsRejectedCurrentRow(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()

	expectQuoteWrites(mock, quote, 0, 1)
	mock.ExpectCommit()

	result, err := store.SaveQuote(context.Background(), quote)
	if err != nil {
		t.Fatalf("SaveQuote returned error: %v", err)
	}
	if result.CurrentAccepted {
		t.Error("CurrentAccepted = true, want false for a precedence-rejected quote")
	}
	if !result.HistoryChanged {
		t.Error("HistoryChanged = false, want true when only the current row was rejected")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

func TestSaveQuoteReportsRejectedHistoryRow(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()

	expectQuoteWrites(mock, quote, 1, 0)
	mock.ExpectCommit()

	result, err := store.SaveQuote(context.Background(), quote)
	if err != nil {
		t.Fatalf("SaveQuote returned error: %v", err)
	}
	if !result.CurrentAccepted {
		t.Error("CurrentAccepted = false, want true when only history was rejected")
	}
	if result.HistoryChanged {
		t.Error("HistoryChanged = true, want false for a precedence-rejected history row")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

func TestSaveQuoteRollsBackWhenHistoryWriteFails(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()

	mock.ExpectBegin()
	mock.ExpectExec(upsertCurrentPriceSQL).
		WithArgs(
			quote.Symbol,
			quote.Price,
			quote.AsOf,
			quote.Source,
			quote.FetchedAt,
			quote.ProviderSymbol,
			quote.Exchange,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(upsertHistorySQL).
		WithArgs(
			quote.Symbol,
			quote.Price,
			quote.AsOf,
			quote.AsOf,
			quote.Source,
			quote.FetchedAt,
			quote.ProviderSymbol,
			quote.Exchange,
		).
		WillReturnError(errors.New("history write failed"))
	mock.ExpectRollback()

	if _, err := store.SaveQuote(context.Background(), quote); err == nil {
		t.Fatal("SaveQuote returned nil error, want history write failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

func TestSaveQuoteRejectsInvalidQuoteBeforeAnySQL(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()
	quote.Source = "unknown_source"

	_, err := store.SaveQuote(context.Background(), quote)
	if err == nil {
		t.Fatal("SaveQuote returned nil error, want validation failure")
	}
	var validationErr *marketdata.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("SaveQuote error = %T %v, want *marketdata.ValidationError", err, err)
	}
	if validationErr.Field != "source" {
		t.Errorf("validation field = %q, want source", validationErr.Field)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("invalid quote should not run SQL: %v", err)
	}
}

func TestSaveQuoteHistoryDateComesFromQuoteTime(t *testing.T) {
	if !strings.Contains(upsertHistorySQL, "market_date($3::TIMESTAMPTZ)") {
		t.Fatalf("history upsert must derive date from the quote timestamp, got SQL: %s", upsertHistorySQL)
	}
	if strings.Contains(upsertHistorySQL, "market_today()") {
		t.Fatal("history upsert must not derive the date from market_today()")
	}
}

func TestSaveQuoteUnchangedPriceStillRefreshesProvenance(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()

	// The price is unchanged but the quote is newer, so both upserts still
	// refresh timestamps and provenance.
	expectQuoteWrites(mock, quote, 1, 1)
	mock.ExpectCommit()

	result, err := store.SaveQuote(context.Background(), quote)
	if err != nil {
		t.Fatalf("SaveQuote returned error: %v", err)
	}
	if !result.CurrentAccepted || !result.HistoryChanged {
		t.Errorf("SaveQuote result = %+v, want both writes accepted", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

func TestSaveQuoteFailsWhenCommitFails(t *testing.T) {
	store, mock := newPriceStoreMock(t)
	quote := priceStoreTestQuote()

	expectQuoteWrites(mock, quote, 1, 1)
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	if _, err := store.SaveQuote(context.Background(), quote); err == nil {
		t.Fatal("SaveQuote returned nil error, want commit failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}
