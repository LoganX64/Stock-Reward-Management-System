package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/marketcal"
)

type fakeHistoricalDaily struct {
	quotes map[string]map[string]Quote
	errs   map[string]error
	calls  []string
}

func (f *fakeHistoricalDaily) Name() string {
	return SourceNSEBhavcopy
}

func (f *fakeHistoricalDaily) GetHistoricalDailyCloses(
	ctx context.Context,
	date time.Time,
	symbols []string,
) (map[string]Quote, error) {
	key := date.Format("2006-01-02")
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	return f.quotes[key], nil
}

func TestBhavcopyHistoryProviderSkipsWeekendsAndReportsMissingFiles(t *testing.T) {
	calendar := marketcal.Default()
	asOf := time.Date(2026, time.October, 30, 15, 30, 0, 0, calendar.Location()).UTC()

	daily := &fakeHistoricalDaily{
		quotes: map[string]map[string]Quote{
			"2026-10-30": {
				"TCS": {
					Symbol:         "TCS",
					ProviderSymbol: "TCS",
					Price:          decimal.RequireFromString("2502.60"),
					Currency:       CurrencyINR,
					Exchange:       ExchangeNSE,
					AsOf:           asOf,
					FetchedAt:      asOf.Add(time.Hour),
					Source:         SourceNSEBhavcopy,
				},
			},
		},
		errs: map[string]error{
			"2026-11-02": ErrNoSession,
		},
	}

	provider, err := NewBhavcopyHistoryProvider(daily, calendar)
	if err != nil {
		t.Fatalf("NewBhavcopyHistoryProvider() error = %v", err)
	}

	from := time.Date(2026, time.October, 30, 0, 0, 0, 0, calendar.Location())
	to := time.Date(2026, time.November, 2, 0, 0, 0, 0, calendar.Location())
	days, err := provider.GetHistory(context.Background(), from, to, []string{"TCS"})
	if err != nil {
		t.Fatalf("GetHistory() error = %v", err)
	}

	if len(days) != 2 {
		t.Fatalf("GetHistory() returned %d days, want 2 trading days", len(days))
	}
	if got := daily.calls; len(got) != 2 || got[0] != "2026-10-30" || got[1] != "2026-11-02" {
		t.Errorf("provider calls = %v, want the Friday and Monday trading days only", got)
	}
	if days[0].Err != nil {
		t.Errorf("Friday error = %v, want nil", days[0].Err)
	}
	if got := days[0].Quotes["TCS"].Price.String(); got != "2502.6" {
		t.Errorf("Friday TCS price = %s, want 2502.6", got)
	}
	if !errors.Is(days[1].Err, ErrNoSession) {
		t.Errorf("Monday error = %v, want ErrNoSession", days[1].Err)
	}
}

func TestBhavcopyHistoryProviderRejectsInvalidRange(t *testing.T) {
	provider, err := NewBhavcopyHistoryProvider(&fakeHistoricalDaily{}, marketcal.Default())
	if err != nil {
		t.Fatalf("NewBhavcopyHistoryProvider() error = %v", err)
	}

	from := time.Date(2026, time.November, 2, 0, 0, 0, 0, marketcal.Location())
	to := time.Date(2026, time.October, 30, 0, 0, 0, 0, marketcal.Location())
	if _, err := provider.GetHistory(context.Background(), from, to, []string{"TCS"}); err == nil {
		t.Fatal("GetHistory() accepted a range whose end precedes its start")
	}
}

func TestNewBhavcopyHistoryProviderRequiresDailyProvider(t *testing.T) {
	if _, err := NewBhavcopyHistoryProvider(nil, marketcal.Default()); err == nil {
		t.Fatal("NewBhavcopyHistoryProvider() accepted a nil daily provider")
	}
}
