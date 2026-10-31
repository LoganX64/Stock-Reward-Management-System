package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
)

type fakeBackfillStore struct {
	entries  []CurrentPrice
	saved    []marketdata.Quote
	changed  map[string]bool
	saveErrs map[string]error
}

func (f *fakeBackfillStore) ListCurrentPrices(ctx context.Context) ([]CurrentPrice, error) {
	return f.entries, nil
}

func (f *fakeBackfillStore) SaveHistoryQuote(ctx context.Context, quote marketdata.Quote) (bool, error) {
	if err := f.saveErrs[quote.Symbol]; err != nil {
		return false, err
	}
	f.saved = append(f.saved, quote)
	return f.changed[quote.Symbol+"|"+quote.AsOf.In(marketcal.Location()).Format("2006-01-02")], nil
}

type fakeBackfillProvider struct {
	days    []marketdata.HistoryDay
	err     error
	from    time.Time
	to      time.Time
	symbols []string
}

func (f *fakeBackfillProvider) Name() string {
	return "fake_history"
}

func (f *fakeBackfillProvider) GetHistory(
	ctx context.Context,
	from, to time.Time,
	symbols []string,
) ([]marketdata.HistoryDay, error) {
	f.from = from
	f.to = to
	f.symbols = append([]string(nil), symbols...)
	return f.days, f.err
}

func backfillTestQuote(symbol string, date time.Time, price string) marketdata.Quote {
	asOf := marketcal.Default().SessionClose(date).UTC()
	return marketdata.Quote{
		Symbol:         symbol,
		ProviderSymbol: symbol,
		Price:          decimal.RequireFromString(price),
		Currency:       marketdata.CurrencyINR,
		Exchange:       marketdata.ExchangeNSE,
		AsOf:           asOf,
		FetchedAt:      asOf.Add(3 * time.Hour),
		Source:         marketdata.SourceNSEBhavcopy,
	}
}

func TestHistoryBackfillImportsRawClosesAndReportsMissingDates(t *testing.T) {
	calendar := marketcal.Default()
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, calendar.Location())
	friday := time.Date(2026, time.October, 30, 0, 0, 0, 0, calendar.Location())
	monday := time.Date(2026, time.November, 2, 0, 0, 0, 0, calendar.Location())

	store := &fakeBackfillStore{
		entries: []CurrentPrice{{Symbol: "TCS"}, {Symbol: "INFY"}},
		changed: map[string]bool{
			"TCS|2026-10-30": true,
		},
	}
	provider := &fakeBackfillProvider{
		days: []marketdata.HistoryDay{
			{
				Date: friday,
				Quotes: map[string]marketdata.Quote{
					"TCS":  backfillTestQuote("TCS", friday, "2502.60"),
					"INFY": backfillTestQuote("INFY", friday, "1185.00"),
				},
			},
			{Date: monday, Err: marketdata.ErrNoSession},
		},
	}

	service, err := NewHistoryBackfillService(store, provider, &HistoryBackfillOptions{
		Calendar: calendar,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewHistoryBackfillService() error = %v", err)
	}

	result, err := service.Run(context.Background(), friday, monday, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Symbols != 2 {
		t.Errorf("Symbols = %d, want 2", result.Symbols)
	}
	if result.TradingDays != 2 {
		t.Errorf("TradingDays = %d, want 2", result.TradingDays)
	}
	if result.QuotesImported != 1 || result.QuotesUnchanged != 1 {
		t.Errorf("imported/unchanged = %d/%d, want 1/1", result.QuotesImported, result.QuotesUnchanged)
	}
	if len(result.MissingDates) != 1 || result.MissingDates[0] != "2026-11-02" {
		t.Errorf("MissingDates = %v, want [2026-11-02]", result.MissingDates)
	}
	if len(result.Failed) != 0 {
		t.Errorf("Failed = %v, want none", result.Failed)
	}
	if got := provider.symbols; len(got) != 2 || got[0] != "INFY" || got[1] != "TCS" {
		t.Errorf("provider symbols = %v, want sorted [INFY TCS]", got)
	}
	if len(store.saved) != 2 {
		t.Fatalf("saved quotes = %d, want 2", len(store.saved))
	}
	if got := store.saved[0].Price.String(); got != "1185" {
		t.Errorf("saved INFY price = %s, want the raw close 1185", got)
	}
	if got := store.saved[1].Price.String(); got != "2502.6" {
		t.Errorf("saved TCS price = %s, want the raw close 2502.6", got)
	}
}

func TestHistoryBackfillCollectsDateAndSymbolFailures(t *testing.T) {
	calendar := marketcal.Default()
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, calendar.Location())
	friday := time.Date(2026, time.October, 30, 0, 0, 0, 0, calendar.Location())
	monday := time.Date(2026, time.November, 2, 0, 0, 0, 0, calendar.Location())

	store := &fakeBackfillStore{
		entries:  []CurrentPrice{{Symbol: "TCS"}},
		saveErrs: map[string]error{"TCS": errors.New("database write failed")},
	}
	provider := &fakeBackfillProvider{
		days: []marketdata.HistoryDay{
			{Date: friday, Err: errors.New("network unavailable")},
			{
				Date: monday,
				Quotes: map[string]marketdata.Quote{
					"TCS": backfillTestQuote("TCS", monday, "2510.00"),
				},
			},
		},
	}

	service, err := NewHistoryBackfillService(store, provider, &HistoryBackfillOptions{
		Calendar: calendar,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewHistoryBackfillService() error = %v", err)
	}

	result, err := service.Run(context.Background(), friday, monday, []string{" tcs "})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Failed) != 2 {
		t.Fatalf("Failed = %v, want one date failure and one symbol failure", result.Failed)
	}
	if result.Failed[0].Date != "2026-10-30" || result.Failed[0].Symbol != "" {
		t.Errorf("date failure = %+v, want the 2026-10-30 fetch failure", result.Failed[0])
	}
	if result.Failed[1].Date != "2026-11-02" || result.Failed[1].Symbol != "TCS" {
		t.Errorf("symbol failure = %+v, want TCS on 2026-11-02", result.Failed[1])
	}
	if result.QuotesImported != 0 || result.QuotesUnchanged != 0 {
		t.Errorf("imported/unchanged = %d/%d, want 0/0", result.QuotesImported, result.QuotesUnchanged)
	}
}

func TestHistoryBackfillRejectsFutureRange(t *testing.T) {
	calendar := marketcal.Default()
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, calendar.Location())
	service, err := NewHistoryBackfillService(&fakeBackfillStore{}, &fakeBackfillProvider{}, &HistoryBackfillOptions{
		Calendar: calendar,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewHistoryBackfillService() error = %v", err)
	}

	from := time.Date(2026, time.November, 2, 0, 0, 0, 0, calendar.Location())
	to := time.Date(2026, time.November, 3, 0, 0, 0, 0, calendar.Location())
	if _, err := service.Run(context.Background(), from, to, []string{"TCS"}); err == nil {
		t.Fatal("Run() accepted a range after market today")
	}
}

func TestHistoryBackfillRequiresSymbols(t *testing.T) {
	calendar := marketcal.Default()
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, calendar.Location())
	service, err := NewHistoryBackfillService(&fakeBackfillStore{}, &fakeBackfillProvider{}, &HistoryBackfillOptions{
		Calendar: calendar,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewHistoryBackfillService() error = %v", err)
	}

	from := time.Date(2026, time.October, 30, 0, 0, 0, 0, calendar.Location())
	if _, err := service.Run(context.Background(), from, from, nil); err == nil {
		t.Fatal("Run() accepted an empty symbol set")
	}
}

func TestNewHistoryBackfillServiceRequiresDependencies(t *testing.T) {
	if _, err := NewHistoryBackfillService(nil, &fakeBackfillProvider{}, nil); err == nil {
		t.Fatal("NewHistoryBackfillService() accepted a nil store")
	}
	if _, err := NewHistoryBackfillService(&fakeBackfillStore{}, nil, nil); err == nil {
		t.Fatal("NewHistoryBackfillService() accepted a nil provider")
	}
}
