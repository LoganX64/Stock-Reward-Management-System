package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
	"github.com/LoganX64/stocky-api/internal/utils"
)

// HistoryBackfillOptions configures the historical import. Now is injectable
// for deterministic range validation in tests.
type HistoryBackfillOptions struct {
	Calendar *marketcal.Calendar
	Now      func() time.Time
}

// HistoryBackfillService imports raw official closes into stock_price_history
// without touching current prices.
type HistoryBackfillService struct {
	store    HistoryStore
	provider marketdata.HistoryProvider
	calendar *marketcal.Calendar
	now      func() time.Time
}

// BackfillFailure records one date or one symbol that could not be imported.
type BackfillFailure struct {
	Date   string `json:"date"`
	Symbol string `json:"symbol,omitempty"`
	Error  string `json:"error"`
}

// HistoryBackfillResult is the operator-facing summary of one run.
type HistoryBackfillResult struct {
	Provider        string            `json:"provider"`
	From            string            `json:"from"`
	To              string            `json:"to"`
	Symbols         int               `json:"symbols"`
	TradingDays     int               `json:"tradingDays"`
	QuotesImported  int               `json:"quotesImported"`
	QuotesUnchanged int               `json:"quotesUnchanged"`
	SymbolsMissing  int               `json:"symbolsMissing"`
	MissingDates    []string          `json:"missingDates"`
	Failed          []BackfillFailure `json:"failed"`
}

// NewHistoryBackfillService creates a backfill service.
func NewHistoryBackfillService(
	store HistoryStore,
	provider marketdata.HistoryProvider,
	opts *HistoryBackfillOptions,
) (*HistoryBackfillService, error) {
	if store == nil {
		return nil, fmt.Errorf("history backfill: price store is required")
	}
	if provider == nil {
		return nil, fmt.Errorf("history backfill: history provider is required")
	}

	calendar := marketcal.Default()
	now := time.Now
	if opts != nil {
		if opts.Calendar != nil {
			calendar = opts.Calendar
		}
		if opts.Now != nil {
			now = opts.Now
		}
	}

	return &HistoryBackfillService{
		store:    store,
		provider: provider,
		calendar: calendar,
		now:      now,
	}, nil
}

// Run imports every trading date in the inclusive range. Missing files are
// reported per date and never replaced with fabricated flat prices. Per-date
// or per-symbol failures are collected in the result so the caller can decide
// whether to retry without losing the successful imports.
func (s *HistoryBackfillService) Run(
	ctx context.Context,
	from, to time.Time,
	symbols []string,
) (HistoryBackfillResult, error) {
	result := HistoryBackfillResult{
		Provider:     s.provider.Name(),
		MissingDates: []string{},
		Failed:       []BackfillFailure{},
	}

	if from.IsZero() || to.IsZero() {
		return result, fmt.Errorf("history backfill: from and to dates are required")
	}

	from = s.calendar.Date(from)
	to = s.calendar.Date(to)
	result.From = from.Format("2006-01-02")
	result.To = to.Format("2006-01-02")

	if to.Before(from) {
		return result, fmt.Errorf(
			"history backfill: to date %s is before from date %s",
			result.To,
			result.From,
		)
	}
	if today := s.calendar.Date(s.now()); to.After(today) {
		return result, fmt.Errorf(
			"history backfill: to date %s is after market today %s",
			result.To,
			today.Format("2006-01-02"),
		)
	}

	normalized, err := normalizeBackfillSymbols(symbols)
	if err != nil {
		return result, err
	}
	if len(normalized) == 0 {
		entries, err := s.store.ListCurrentPrices(ctx)
		if err != nil {
			return result, fmt.Errorf("history backfill: list tracked symbols: %w", err)
		}
		normalized, err = normalizeBackfillSymbols(symbolsFromCurrentPrices(entries))
		if err != nil {
			return result, err
		}
	}
	if len(normalized) == 0 {
		return result, fmt.Errorf("history backfill: no symbols to import")
	}
	result.Symbols = len(normalized)

	started := s.now()
	log := logrus.WithFields(logrus.Fields{
		"provider": result.Provider,
		"from":     result.From,
		"to":       result.To,
		"symbols":  result.Symbols,
	})
	log.Info("History backfill started")

	days, err := s.provider.GetHistory(ctx, from, to, normalized)
	if err != nil {
		return result, fmt.Errorf("history backfill: fetch range: %w", err)
	}

	for _, day := range days {
		dateText := day.Date.Format("2006-01-02")
		result.TradingDays++

		if day.Err != nil {
			if errors.Is(day.Err, marketdata.ErrNoSession) {
				result.MissingDates = append(result.MissingDates, dateText)
				log.WithFields(logrus.Fields{
					"event":       "history_date_missing",
					"market_date": dateText,
				}).Warn("No official bhavcopy file for this date -- leaving an explicit gap")
			} else {
				result.Failed = append(result.Failed, BackfillFailure{
					Date:  dateText,
					Error: day.Err.Error(),
				})
				log.WithError(day.Err).WithFields(logrus.Fields{
					"event":       "history_date_failed",
					"market_date": dateText,
				}).Error("History import failed for this date -- continuing with the range")
			}
			continue
		}

		daySymbols := make([]string, 0, len(day.Quotes))
		for symbol := range day.Quotes {
			daySymbols = append(daySymbols, symbol)
		}
		sort.Strings(daySymbols)

		if missing := len(normalized) - len(day.Quotes); missing > 0 {
			result.SymbolsMissing += missing
			log.WithFields(logrus.Fields{
				"event":       "history_symbols_missing",
				"market_date": dateText,
				"missing":     missing,
			}).Warn("Some requested symbols are absent from the bhavcopy file -- no row will be fabricated")
		}

		for _, symbol := range daySymbols {
			quote := day.Quotes[symbol]
			changed, err := s.store.SaveHistoryQuote(ctx, quote)
			if err != nil {
				result.Failed = append(result.Failed, BackfillFailure{
					Date:   dateText,
					Symbol: symbol,
					Error:  err.Error(),
				})
				log.WithError(err).WithFields(logrus.Fields{
					"event":       "history_quote_failed",
					"market_date": dateText,
					"symbol":      symbol,
				}).Error("Historical quote save failed")
				continue
			}
			if changed {
				result.QuotesImported++
			} else {
				result.QuotesUnchanged++
			}
		}
	}

	log.WithFields(logrus.Fields{
		"event":            "history_backfill_completed",
		"trading_days":     result.TradingDays,
		"quotes_imported":  result.QuotesImported,
		"quotes_unchanged": result.QuotesUnchanged,
		"symbols_missing":  result.SymbolsMissing,
		"missing_dates":    len(result.MissingDates),
		"failed":           len(result.Failed),
		"duration_ms":      s.now().Sub(started).Milliseconds(),
	}).Info("History backfill completed")

	return result, nil
}

func normalizeBackfillSymbols(symbols []string) ([]string, error) {
	normalized := make([]string, 0, len(symbols))
	seen := make(map[string]struct{}, len(symbols))
	for _, raw := range symbols {
		symbol := utils.NormalizeStockSymbol(raw)
		if symbol == "" {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		normalized = append(normalized, symbol)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func symbolsFromCurrentPrices(entries []CurrentPrice) []string {
	symbols := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Symbol != "" {
			symbols = append(symbols, entry.Symbol)
		}
	}
	return symbols
}
