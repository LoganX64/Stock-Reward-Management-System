package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
)

type fakeDailyCloseProvider struct {
	quotes  map[string]marketdata.Quote
	err     error
	calls   int
	dates   []time.Time
	symbols [][]string
}

func (f *fakeDailyCloseProvider) Name() string {
	return marketdata.SourceNSEBhavcopy
}

func (f *fakeDailyCloseProvider) GetDailyCloses(
	ctx context.Context,
	date time.Time,
	symbols []string,
) (map[string]marketdata.Quote, error) {
	f.calls++
	f.dates = append(f.dates, date)
	f.symbols = append(f.symbols, append([]string(nil), symbols...))
	if f.err != nil {
		return nil, f.err
	}
	return f.quotes, nil
}

type bhavcopyTestStore struct {
	current    []CurrentPrice
	listErr    error
	result     SaveResult
	saveErrors map[string]error
	saved      []marketdata.Quote
}

func (f *bhavcopyTestStore) ListCurrentPrices(ctx context.Context) ([]CurrentPrice, error) {
	return f.current, f.listErr
}

func (f *bhavcopyTestStore) SaveQuote(ctx context.Context, quote marketdata.Quote) (SaveResult, error) {
	if err, ok := f.saveErrors[quote.Symbol]; ok {
		return SaveResult{}, err
	}
	f.saved = append(f.saved, quote)
	return f.result, nil
}

func bhavcopyTestQuote(symbol, price string, date time.Time) marketdata.Quote {
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

func newTestBhavcopyService(
	t *testing.T,
	store PriceStore,
	provider marketdata.DailyCloseProvider,
	now time.Time,
) *BhavcopyService {
	t.Helper()

	service, err := NewBhavcopyService(store, provider, &BhavcopyServiceOptions{
		Calendar: marketcal.Default(),
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewBhavcopyService() error = %v", err)
	}
	return service
}

func TestBhavcopyServiceRunAttemptSavesQuotes(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{
		current: []CurrentPrice{
			{Symbol: "TCS", Source: marketdata.SourceYahoo},
			{Symbol: "INFY", Source: marketdata.SourceYahoo},
			{Symbol: "RELIANCE", Source: marketdata.SourceYahoo},
		},
		result: SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeDailyCloseProvider{quotes: map[string]marketdata.Quote{
		"TCS":      bhavcopyTestQuote("TCS", "2502.60", date),
		"INFY":     bhavcopyTestQuote("INFY", "1185.00", date),
		"RELIANCE": bhavcopyTestQuote("RELIANCE", "1164.90", date),
	}}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); !ok {
		t.Fatal("runAttempt() = false, want true")
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
	if len(provider.symbols) != 1 || len(provider.symbols[0]) != 3 {
		t.Errorf("provider symbols = %v, want all three tracked symbols", provider.symbols)
	}
	if len(store.saved) != 3 {
		t.Fatalf("saved quotes = %d, want 3", len(store.saved))
	}

	want := map[string]string{
		"TCS":      "2502.60",
		"INFY":     "1185.00",
		"RELIANCE": "1164.90",
	}
	for _, quote := range store.saved {
		if !quote.Price.Equal(decimal.RequireFromString(want[quote.Symbol])) {
			t.Errorf("%s saved price = %s, want %s", quote.Symbol, quote.Price, want[quote.Symbol])
		}
		if quote.Source != marketdata.SourceNSEBhavcopy {
			t.Errorf("%s source = %q, want %s", quote.Symbol, quote.Source, marketdata.SourceNSEBhavcopy)
		}
	}
}

func TestBhavcopyServiceRunAttemptMissingFileWritesNothing(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{
		current: []CurrentPrice{{Symbol: "TCS"}},
		result:  SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeDailyCloseProvider{
		err: fmt.Errorf("%w: test 404", marketdata.ErrNoSession),
	}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); ok {
		t.Fatal("runAttempt() = true, want false for a missing file")
	}
	if len(store.saved) != 0 {
		t.Errorf("saved quotes = %d, want 0", len(store.saved))
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
}

func TestBhavcopyServiceRunAttemptProviderErrorWritesNothing(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{
		current: []CurrentPrice{{Symbol: "TCS"}},
	}
	provider := &fakeDailyCloseProvider{err: errors.New("network unavailable")}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); ok {
		t.Fatal("runAttempt() = true, want false for a provider error")
	}
	if len(store.saved) != 0 {
		t.Errorf("saved quotes = %d, want 0", len(store.saved))
	}
}

func TestBhavcopyServiceRunAttemptKeepsPricesForMissingSymbols(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{
		current: []CurrentPrice{
			{Symbol: "TCS"},
			{Symbol: "INFY"},
			{Symbol: "RELIANCE"},
		},
		result: SaveResult{CurrentAccepted: true, HistoryChanged: true},
	}
	provider := &fakeDailyCloseProvider{quotes: map[string]marketdata.Quote{
		"TCS": bhavcopyTestQuote("TCS", "2502.60", date),
	}}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); !ok {
		t.Fatal("runAttempt() = false, want true when the file is valid but partial")
	}
	if len(store.saved) != 1 || store.saved[0].Symbol != "TCS" {
		t.Errorf("saved quotes = %+v, want only TCS", store.saved)
	}
}

func TestBhavcopyServiceRunAttemptSaveErrorRequestsRetry(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{
		current: []CurrentPrice{
			{Symbol: "TCS"},
			{Symbol: "INFY"},
			{Symbol: "RELIANCE"},
		},
		result:     SaveResult{CurrentAccepted: true, HistoryChanged: true},
		saveErrors: map[string]error{"INFY": errors.New("database unavailable")},
	}
	provider := &fakeDailyCloseProvider{quotes: map[string]marketdata.Quote{
		"TCS":      bhavcopyTestQuote("TCS", "2502.60", date),
		"INFY":     bhavcopyTestQuote("INFY", "1185.00", date),
		"RELIANCE": bhavcopyTestQuote("RELIANCE", "1164.90", date),
	}}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); ok {
		t.Fatal("runAttempt() = true, want false when a save failed")
	}
	if len(store.saved) != 2 {
		t.Errorf("saved quotes = %d, want the two successful saves", len(store.saved))
	}
}

func TestBhavcopyServiceRunAttemptNoTrackedSymbolsSkipsFetch(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{}
	provider := &fakeDailyCloseProvider{}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); !ok {
		t.Fatal("runAttempt() = false, want true when there is nothing to do")
	}
	if provider.calls != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls)
	}
}

func TestBhavcopyServiceRunAttemptListErrorRequestsRetry(t *testing.T) {
	date := istTime(2026, time.October, 30, 0, 0)
	now := istTime(2026, time.October, 30, 18, 15)
	store := &bhavcopyTestStore{listErr: errors.New("query failed")}
	provider := &fakeDailyCloseProvider{}
	service := newTestBhavcopyService(t, store, provider, now)

	if ok := service.runAttempt(context.Background(), date); ok {
		t.Fatal("runAttempt() = true, want false when the symbol list failed")
	}
	if provider.calls != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls)
	}
}

func TestBhavcopyServiceFirstSchedule(t *testing.T) {
	service := newTestBhavcopyService(t, &bhavcopyTestStore{}, &fakeDailyCloseProvider{}, time.Now())

	tests := []struct {
		name     string
		now      time.Time
		wantDate time.Time
		wantAt   time.Time
	}{
		{
			name:     "before first attempt waits for 18:15",
			now:      istTime(2026, time.October, 30, 17, 0),
			wantDate: istTime(2026, time.October, 30, 0, 0),
			wantAt:   istTime(2026, time.October, 30, 18, 15),
		},
		{
			name:     "at first attempt runs now",
			now:      istTime(2026, time.October, 30, 18, 15),
			wantDate: istTime(2026, time.October, 30, 0, 0),
			wantAt:   istTime(2026, time.October, 30, 18, 15),
		},
		{
			name:     "startup inside window catches up now",
			now:      istTime(2026, time.October, 30, 19, 5),
			wantDate: istTime(2026, time.October, 30, 0, 0),
			wantAt:   istTime(2026, time.October, 30, 19, 5),
		},
		{
			name:     "startup after window still catches up once",
			now:      istTime(2026, time.October, 30, 21, 0),
			wantDate: istTime(2026, time.October, 30, 0, 0),
			wantAt:   istTime(2026, time.October, 30, 21, 0),
		},
		{
			name:     "weekend waits for Monday",
			now:      istTime(2026, time.October, 31, 12, 0),
			wantDate: istTime(2026, time.November, 2, 0, 0),
			wantAt:   istTime(2026, time.November, 2, 18, 15),
		},
		{
			name:     "holiday waits for the next trading day",
			now:      istTime(2026, time.November, 10, 10, 0),
			wantDate: istTime(2026, time.November, 11, 0, 0),
			wantAt:   istTime(2026, time.November, 11, 18, 15),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := service.firstSchedule(tt.now)
			if !got.date.Equal(tt.wantDate) {
				t.Errorf("date = %s, want %s", got.date, tt.wantDate)
			}
			if !got.at.Equal(tt.wantAt) {
				t.Errorf("at = %s, want %s", got.at, tt.wantAt)
			}
		})
	}
}

func TestBhavcopyServiceRetrySchedule(t *testing.T) {
	service := newTestBhavcopyService(t, &bhavcopyTestStore{}, &fakeDailyCloseProvider{}, time.Now())

	tests := []struct {
		name        string
		date        time.Time
		attemptedAt time.Time
		wantDate    time.Time
		wantAt      time.Time
	}{
		{
			name:        "first failure retries at 18:45",
			date:        istTime(2026, time.October, 30, 0, 0),
			attemptedAt: istTime(2026, time.October, 30, 18, 15),
			wantDate:    istTime(2026, time.October, 30, 0, 0),
			wantAt:      istTime(2026, time.October, 30, 18, 45),
		},
		{
			name:        "catch-up failure aligns to the next grid slot",
			date:        istTime(2026, time.October, 30, 0, 0),
			attemptedAt: istTime(2026, time.October, 30, 18, 20),
			wantDate:    istTime(2026, time.October, 30, 0, 0),
			wantAt:      istTime(2026, time.October, 30, 18, 45),
		},
		{
			name:        "last in-window failure moves to the next trading day",
			date:        istTime(2026, time.October, 30, 0, 0),
			attemptedAt: istTime(2026, time.October, 30, 20, 15),
			wantDate:    istTime(2026, time.November, 2, 0, 0),
			wantAt:      istTime(2026, time.November, 2, 18, 15),
		},
		{
			name:        "after-window catch-up failure moves to the next trading day",
			date:        istTime(2026, time.October, 30, 0, 0),
			attemptedAt: istTime(2026, time.October, 30, 21, 0),
			wantDate:    istTime(2026, time.November, 2, 0, 0),
			wantAt:      istTime(2026, time.November, 2, 18, 15),
		},
		{
			name:        "Friday failure after the window waits for Monday",
			date:        istTime(2026, time.October, 30, 0, 0),
			attemptedAt: istTime(2026, time.October, 30, 20, 45),
			wantDate:    istTime(2026, time.November, 2, 0, 0),
			wantAt:      istTime(2026, time.November, 2, 18, 15),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := service.retrySchedule(tt.date, tt.attemptedAt)
			if !got.date.Equal(tt.wantDate) {
				t.Errorf("date = %s, want %s", got.date, tt.wantDate)
			}
			if !got.at.Equal(tt.wantAt) {
				t.Errorf("at = %s, want %s", got.at, tt.wantAt)
			}
		})
	}
}

func TestNewBhavcopyServiceValidation(t *testing.T) {
	store := &bhavcopyTestStore{}
	provider := &fakeDailyCloseProvider{}

	tests := []struct {
		name string
		opts *BhavcopyServiceOptions
	}{
		{
			name: "invalid first attempt clock",
			opts: &BhavcopyServiceOptions{FirstAttempt: "8:15"},
		},
		{
			name: "inverted retry window",
			opts: &BhavcopyServiceOptions{FirstAttempt: "20:30", RetryUntil: "18:15"},
		},
		{
			name: "negative retry interval",
			opts: &BhavcopyServiceOptions{RetryInterval: -time.Minute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewBhavcopyService(store, provider, tt.opts); err == nil {
				t.Fatal("NewBhavcopyService() accepted invalid options")
			}
		})
	}

	if _, err := NewBhavcopyService(nil, provider, nil); err == nil {
		t.Error("NewBhavcopyService() accepted a nil store")
	}
	if _, err := NewBhavcopyService(store, nil, nil); err == nil {
		t.Error("NewBhavcopyService() accepted a nil provider")
	}
}

func TestBhavcopyServiceStartStopsOnContextCancellation(t *testing.T) {
	service := newTestBhavcopyService(
		t,
		&bhavcopyTestStore{},
		&fakeDailyCloseProvider{},
		time.Now(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Start(ctx)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not stop after context cancellation")
	}
}
