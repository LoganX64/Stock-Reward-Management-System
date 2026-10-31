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

var updaterTestNow = time.Date(2026, time.October, 30, 10, 5, 0, 0, time.UTC)

func updaterTestQuote() marketdata.Quote {
	return marketdata.Quote{
		Symbol:         "TCS",
		ProviderSymbol: "TCS.NS",
		Price:          decimal.RequireFromString("2502.60"),
		Currency:       marketdata.CurrencyINR,
		Exchange:       marketdata.ExchangeNSE,
		AsOf:           updaterTestNow.Add(-5 * time.Minute),
		FetchedAt:      updaterTestNow,
		Source:         marketdata.SourceYahoo,
	}
}

type fakeQuoteProvider struct {
	quote   marketdata.Quote
	err     error
	symbols []string
}

func (f *fakeQuoteProvider) Name() string {
	return "fake-yahoo"
}

func (f *fakeQuoteProvider) GetQuote(ctx context.Context, symbol string) (marketdata.Quote, error) {
	f.symbols = append(f.symbols, symbol)
	if f.err != nil {
		return marketdata.Quote{}, f.err
	}
	return f.quote, nil
}

type fakePriceStore struct {
	current []CurrentPrice
	listErr error
	result  SaveResult
	saveErr error
	saved   []marketdata.Quote
}

func (f *fakePriceStore) ListCurrentPrices(ctx context.Context) ([]CurrentPrice, error) {
	return f.current, f.listErr
}

func (f *fakePriceStore) SaveQuote(ctx context.Context, quote marketdata.Quote) (SaveResult, error) {
	f.saved = append(f.saved, quote)
	return f.result, f.saveErr
}

func newTestPriceService(provider marketdata.QuoteProvider, store PriceStore) (*PriceService, *PriceCache) {
	cache := NewPriceCache()
	service := NewPriceService(store, cache, provider, &PriceServiceOptions{
		Now: func() time.Time { return updaterTestNow },
	})
	return service, cache
}

func TestProcessJobProviderErrorWritesNothing(t *testing.T) {
	store := &fakePriceStore{}
	provider := &fakeQuoteProvider{err: errors.New("provider unavailable")}
	service, cache := newTestPriceService(provider, store)

	oldAsOf := updaterTestNow.Add(-time.Hour)
	cache.SetPrice("TCS", decimal.RequireFromString("2400"), oldAsOf)

	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: decimal.RequireFromString("2400"),
	})

	if len(store.saved) != 0 {
		t.Errorf("provider error wrote %d quotes, want 0", len(store.saved))
	}
	price, asOf, ok := cache.GetPrice("TCS")
	if !ok {
		t.Fatal("cache entry disappeared after provider error")
	}
	if !price.Equal(decimal.RequireFromString("2400")) || !asOf.Equal(oldAsOf) {
		t.Errorf("cache = (%s, %s), want (2400, %s)", price, asOf, oldAsOf)
	}
	if len(provider.symbols) != 1 || provider.symbols[0] != "TCS" {
		t.Errorf("provider symbols = %v, want [TCS]", provider.symbols)
	}
}

func TestProcessJobAcceptedQuoteSavesAndCaches(t *testing.T) {
	store := &fakePriceStore{
		result: SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeQuoteProvider{quote: updaterTestQuote()}
	service, cache := newTestPriceService(provider, store)

	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: decimal.RequireFromString("2400"),
	})

	if len(store.saved) != 1 {
		t.Fatalf("saved %d quotes, want 1", len(store.saved))
	}
	if !store.saved[0].Price.Equal(decimal.RequireFromString("2502.60")) {
		t.Errorf("saved price = %s, want 2502.60", store.saved[0].Price)
	}
	price, asOf, ok := cache.GetPrice("TCS")
	if !ok {
		t.Fatal("cache entry missing after accepted quote")
	}
	if !price.Equal(decimal.RequireFromString("2502.60")) {
		t.Errorf("cache price = %s, want 2502.60", price)
	}
	if !asOf.Equal(updaterTestQuote().AsOf) {
		t.Errorf("cache asOf = %s, want %s", asOf, updaterTestQuote().AsOf)
	}
}

func TestProcessJobUnchangedPriceStillSavesAndRefreshesCache(t *testing.T) {
	quote := updaterTestQuote()
	store := &fakePriceStore{
		result: SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeQuoteProvider{quote: quote}
	service, cache := newTestPriceService(provider, store)

	oldAsOf := quote.AsOf.Add(-time.Hour)
	cache.SetPrice("TCS", quote.Price, oldAsOf)

	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: quote.Price,
	})

	if len(store.saved) != 1 {
		t.Fatalf("saved %d quotes, want 1", len(store.saved))
	}
	price, asOf, ok := cache.GetPrice("TCS")
	if !ok {
		t.Fatal("cache entry missing after accepted quote")
	}
	if !price.Equal(quote.Price) {
		t.Errorf("cache price = %s, want %s", price, quote.Price)
	}
	if !asOf.Equal(quote.AsOf) {
		t.Errorf("cache asOf = %s, want %s", asOf, quote.AsOf)
	}
}

func TestProcessJobRejectedQuoteDoesNotUpdateCache(t *testing.T) {
	quote := updaterTestQuote()
	store := &fakePriceStore{
		result: SaveResult{CurrentAccepted: false, HistoryChanged: true},
	}
	provider := &fakeQuoteProvider{quote: quote}
	service, cache := newTestPriceService(provider, store)

	oldPrice := decimal.RequireFromString("2400")
	oldAsOf := quote.AsOf.Add(-time.Hour)
	cache.SetPrice("TCS", oldPrice, oldAsOf)

	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: oldPrice,
	})

	if len(store.saved) != 1 {
		t.Fatalf("saved %d quotes, want 1", len(store.saved))
	}
	price, asOf, ok := cache.GetPrice("TCS")
	if !ok {
		t.Fatal("cache entry missing after rejected quote")
	}
	if !price.Equal(oldPrice) || !asOf.Equal(oldAsOf) {
		t.Errorf("cache = (%s, %s), want (%s, %s)", price, asOf, oldPrice, oldAsOf)
	}
}

func TestProcessJobStoreErrorDoesNotUpdateCache(t *testing.T) {
	quote := updaterTestQuote()
	store := &fakePriceStore{
		result:  SaveResult{CurrentAccepted: true, HistoryChanged: true},
		saveErr: errors.New("database unavailable"),
	}
	provider := &fakeQuoteProvider{quote: quote}
	service, cache := newTestPriceService(provider, store)

	oldPrice := decimal.RequireFromString("2400")
	oldAsOf := quote.AsOf.Add(-time.Hour)
	cache.SetPrice("TCS", oldPrice, oldAsOf)

	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: oldPrice,
	})

	if len(store.saved) != 1 {
		t.Fatalf("saved %d quotes, want 1", len(store.saved))
	}
	price, asOf, ok := cache.GetPrice("TCS")
	if !ok {
		t.Fatal("cache entry missing after store error")
	}
	if !price.Equal(oldPrice) || !asOf.Equal(oldAsOf) {
		t.Errorf("cache = (%s, %s), want (%s, %s)", price, asOf, oldPrice, oldAsOf)
	}
}

func TestProcessJobRejectsUnexpectedProviderSymbol(t *testing.T) {
	quote := updaterTestQuote()
	quote.Symbol = "INFY"
	store := &fakePriceStore{
		result: SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeQuoteProvider{quote: quote}
	service, cache := newTestPriceService(provider, store)

	service.processJob(context.Background(), PriceJob{
		Symbol:   "TCS",
		OldPrice: decimal.RequireFromString("2400"),
	})

	if len(store.saved) != 0 {
		t.Errorf("saved %d quotes for mismatched symbol, want 0", len(store.saved))
	}
	if _, _, ok := cache.GetPrice("TCS"); ok {
		t.Error("cache was updated for a mismatched provider symbol")
	}
}

func TestProcessJobNormalizesSymbolBeforeStore(t *testing.T) {
	quote := updaterTestQuote()
	store := &fakePriceStore{
		result: SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeQuoteProvider{quote: quote}
	service, cache := newTestPriceService(provider, store)

	service.processJob(context.Background(), PriceJob{
		Symbol:   " tcs ",
		OldPrice: decimal.RequireFromString("2400"),
	})

	if len(provider.symbols) != 1 || provider.symbols[0] != "TCS" {
		t.Errorf("provider symbols = %v, want [TCS]", provider.symbols)
	}
	if len(store.saved) != 1 || store.saved[0].Symbol != "TCS" {
		t.Errorf("saved quotes = %+v, want one TCS quote", store.saved)
	}
	if _, _, ok := cache.GetPrice("TCS"); !ok {
		t.Error("cache entry missing for normalized symbol")
	}
}

func TestNextScheduledRun(t *testing.T) {
	calendar := marketcal.Default()

	tests := []struct {
		name            string
		now             time.Time
		marketHoursOnly bool
		calendar        *marketcal.Calendar
		want            time.Time
	}{
		{
			name:            "disabled market-hours mode uses tick interval",
			now:             istTime(2026, time.November, 2, 10, 0),
			marketHoursOnly: false,
			want:            istTime(2026, time.November, 2, 11, 0),
		},
		{
			name:            "before open waits for open",
			now:             istTime(2026, time.November, 2, 8, 0),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 9, 15),
		},
		{
			name:            "at open waits one interval",
			now:             istTime(2026, time.November, 2, 9, 15),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 10, 15),
		},
		{
			name:            "mid-session aligns to next tick",
			now:             istTime(2026, time.November, 2, 10, 20),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 11, 15),
		},
		{
			name:            "last regular tick goes to final quote",
			now:             istTime(2026, time.November, 2, 15, 20),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 15, 35),
		},
		{
			name:            "at close goes to final quote",
			now:             istTime(2026, time.November, 2, 15, 30),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 15, 35),
		},
		{
			name:            "after final quote waits for next session",
			now:             istTime(2026, time.November, 2, 15, 35),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 3, 9, 15),
		},
		{
			name:            "Friday after final quote waits for Monday",
			now:             istTime(2026, time.October, 30, 15, 35),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 9, 15),
		},
		{
			name:            "Saturday waits for Monday",
			now:             istTime(2026, time.October, 31, 12, 0),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 2, 9, 15),
		},
		{
			name:            "holiday waits for next trading day",
			now:             istTime(2026, time.November, 10, 10, 0),
			marketHoursOnly: true,
			want:            istTime(2026, time.November, 11, 9, 15),
		},
		{
			name:            "custom calendar override",
			now:             istTime(2026, time.November, 2, 16, 0),
			marketHoursOnly: true,
			calendar:        mustCalendar(t, []string{"2026-11-03"}),
			want:            istTime(2026, time.November, 4, 9, 15),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cal := calendar
			if tt.calendar != nil {
				cal = tt.calendar
			}
			service := NewPriceService(&fakePriceStore{}, NewPriceCache(), &fakeQuoteProvider{}, &PriceServiceOptions{
				MarketHoursOnly: tt.marketHoursOnly,
				Calendar:        cal,
				Now:             func() time.Time { return tt.now },
			})

			got := service.nextScheduledRun(tt.now)
			if !got.Equal(tt.want) {
				t.Errorf("nextScheduledRun(%s) = %s, want %s", tt.now.Format(time.RFC3339), got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
		})
	}
}

func TestShouldCatchUp(t *testing.T) {
	calendar := marketcal.Default()
	fridayClose := istTime(2026, time.October, 30, 15, 30)

	tests := []struct {
		name            string
		now             time.Time
		marketHoursOnly bool
		latest          CurrentPrice
		hasLatest       bool
		want            bool
	}{
		{
			name:            "market-hours mode disabled always catches up",
			now:             istTime(2026, time.November, 2, 8, 0),
			marketHoursOnly: false,
			want:            true,
		},
		{
			name:            "weekend does not catch up",
			now:             istTime(2026, time.October, 31, 12, 0),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketdata.SourceYahoo, AsOf: fridayClose},
			hasLatest:       true,
			want:            false,
		},
		{
			name:            "before open does not catch up",
			now:             istTime(2026, time.November, 2, 8, 0),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketdata.SourceYahoo, AsOf: fridayClose},
			hasLatest:       true,
			want:            false,
		},
		{
			name:            "missing quote catches up during session",
			now:             istTime(2026, time.November, 2, 10, 0),
			marketHoursOnly: true,
			want:            true,
		},
		{
			name:            "stale Friday close catches up Monday",
			now:             istTime(2026, time.November, 2, 10, 0),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketdata.SourceYahoo, AsOf: fridayClose},
			hasLatest:       true,
			want:            true,
		},
		{
			name:            "fresh quote does not catch up",
			now:             istTime(2026, time.November, 2, 10, 0),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketdata.SourceYahoo, AsOf: istTime(2026, time.November, 2, 9, 30)},
			hasLatest:       true,
			want:            false,
		},
		{
			name:            "recent close is fresh just after close",
			now:             istTime(2026, time.November, 2, 15, 31),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketdata.SourceYahoo, AsOf: istTime(2026, time.November, 2, 15, 15)},
			hasLatest:       true,
			want:            false,
		},
		{
			name:            "stale quote catches up just after close",
			now:             istTime(2026, time.November, 2, 15, 31),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketdata.SourceYahoo, AsOf: istTime(2026, time.November, 2, 14, 0)},
			hasLatest:       true,
			want:            true,
		},
		{
			name:            "legacy quote catches up",
			now:             istTime(2026, time.November, 2, 15, 31),
			marketHoursOnly: true,
			latest:          CurrentPrice{Source: marketcal.SourceLegacy, AsOf: istTime(2026, time.November, 2, 15, 20)},
			hasLatest:       true,
			want:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewPriceService(&fakePriceStore{}, NewPriceCache(), &fakeQuoteProvider{}, &PriceServiceOptions{
				MarketHoursOnly: tt.marketHoursOnly,
				Calendar:        calendar,
				Now:             func() time.Time { return tt.now },
			})

			if got := service.shouldCatchUp(tt.now, tt.latest, tt.hasLatest); got != tt.want {
				t.Errorf("shouldCatchUp() = %v, want %v", got, tt.want)
			}
		})
	}
}

func istTime(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, marketcal.Location())
}

func mustCalendar(t *testing.T, holidays []string) *marketcal.Calendar {
	t.Helper()

	calendar, err := marketcal.DefaultWithOverrides(holidays)
	if err != nil {
		t.Fatalf("DefaultWithOverrides() error = %v", err)
	}
	return calendar
}
