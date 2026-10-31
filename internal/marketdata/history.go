package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/LoganX64/stocky-api/internal/marketcal"
)

// HistoryDay is one market date returned by a HistoryProvider. Err is set when
// the date could not be imported. A range backfill continues after a per-date
// failure so gaps stay explicit instead of aborting the whole run.
type HistoryDay struct {
	Date   time.Time
	Quotes map[string]Quote
	Err    error
}

// HistoryProvider returns archived closes for every trading date in a range.
type HistoryProvider interface {
	Name() string
	GetHistory(ctx context.Context, from, to time.Time, symbols []string) ([]HistoryDay, error)
}

// BhavcopyHistoryProvider walks the NSE calendar and imports one official
// bhavcopy file per trading date. Non-trading days are skipped entirely, and a
// missing file is reported as ErrNoSession for that date.
type BhavcopyHistoryProvider struct {
	daily    HistoricalDailyCloseProvider
	calendar *marketcal.Calendar
}

var _ HistoryProvider = (*BhavcopyHistoryProvider)(nil)

// NewBhavcopyHistoryProvider creates a history provider around an archived
// daily-close provider. A nil calendar defaults to the shared NSE calendar.
func NewBhavcopyHistoryProvider(
	daily HistoricalDailyCloseProvider,
	calendar *marketcal.Calendar,
) (*BhavcopyHistoryProvider, error) {
	if daily == nil {
		return nil, fmt.Errorf("history: daily close provider is required")
	}
	if calendar == nil {
		calendar = marketcal.Default()
	}
	return &BhavcopyHistoryProvider{daily: daily, calendar: calendar}, nil
}

// Name returns the underlying official provider name.
func (p *BhavcopyHistoryProvider) Name() string {
	return p.daily.Name()
}

// GetHistory returns one entry per trading date in the inclusive range. Dates
// with no published file carry ErrNoSession; other fetch or parse failures are
// recorded on their date and the walk continues.
func (p *BhavcopyHistoryProvider) GetHistory(
	ctx context.Context,
	from, to time.Time,
	symbols []string,
) ([]HistoryDay, error) {
	if from.IsZero() || to.IsZero() {
		return nil, fmt.Errorf("history: from and to dates are required")
	}

	from = p.calendar.Date(from)
	to = p.calendar.Date(to)
	if to.Before(from) {
		return nil, fmt.Errorf(
			"history: to date %s is before from date %s",
			to.Format("2006-01-02"),
			from.Format("2006-01-02"),
		)
	}

	days := make([]HistoryDay, 0)
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return days, err
		}
		if !p.calendar.IsTradingDay(day) {
			continue
		}

		quotes, err := p.daily.GetHistoricalDailyCloses(ctx, day, symbols)
		if err != nil {
			days = append(days, HistoryDay{Date: day, Err: err})
			continue
		}
		days = append(days, HistoryDay{Date: day, Quotes: quotes})
	}

	return days, nil
}
