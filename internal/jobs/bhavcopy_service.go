package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
)

const (
	defaultBhavcopyFirstAttempt  = "18:15"
	defaultBhavcopyRetryUntil    = "20:30"
	defaultBhavcopyRetryInterval = 30 * time.Minute
)

// BhavcopyServiceOptions controls the NSE official EOD ingestion schedule.
// FirstAttempt and RetryUntil use HH:MM in the market calendar's timezone.
type BhavcopyServiceOptions struct {
	Calendar      *marketcal.Calendar
	FirstAttempt  string
	RetryUntil    string
	RetryInterval time.Duration
	Now           func() time.Time
}

// BhavcopyService fetches the official NSE bhavcopy once per trading day and
// writes it through PriceStore. It retries within the configured evening
// window and never deletes prices for symbols that are missing from the file.
type BhavcopyService struct {
	store    PriceStore
	provider marketdata.DailyCloseProvider
	opts     BhavcopyServiceOptions

	firstMinute int
	lastMinute  int
}

type bhavcopySchedule struct {
	date time.Time
	at   time.Time
}

// NewBhavcopyService creates a service. Nil options use the A3 defaults.
func NewBhavcopyService(
	store PriceStore,
	provider marketdata.DailyCloseProvider,
	opts *BhavcopyServiceOptions,
) (*BhavcopyService, error) {
	if store == nil {
		return nil, fmt.Errorf("bhavcopy: price store is required")
	}
	if provider == nil {
		return nil, fmt.Errorf("bhavcopy: daily close provider is required")
	}

	o := BhavcopyServiceOptions{
		Calendar:      marketcal.Default(),
		FirstAttempt:  defaultBhavcopyFirstAttempt,
		RetryUntil:    defaultBhavcopyRetryUntil,
		RetryInterval: defaultBhavcopyRetryInterval,
		Now:           time.Now,
	}
	if opts != nil {
		if opts.Calendar != nil {
			o.Calendar = opts.Calendar
		}
		if first := strings.TrimSpace(opts.FirstAttempt); first != "" {
			o.FirstAttempt = first
		}
		if until := strings.TrimSpace(opts.RetryUntil); until != "" {
			o.RetryUntil = until
		}
		if opts.RetryInterval < 0 {
			return nil, fmt.Errorf("bhavcopy: retry interval must not be negative")
		}
		if opts.RetryInterval > 0 {
			o.RetryInterval = opts.RetryInterval
		}
		if opts.Now != nil {
			o.Now = opts.Now
		}
	}

	first, err := parseBhavcopyClock("first attempt", o.FirstAttempt)
	if err != nil {
		return nil, err
	}
	last, err := parseBhavcopyClock("retry until", o.RetryUntil)
	if err != nil {
		return nil, err
	}
	if last < first {
		return nil, fmt.Errorf(
			"bhavcopy: retry until %q must not be before first attempt %q",
			o.RetryUntil,
			o.FirstAttempt,
		)
	}

	return &BhavcopyService{
		store:       store,
		provider:    provider,
		opts:        o,
		firstMinute: first,
		lastMinute:  last,
	}, nil
}

// Start runs the calendar-aware bhavcopy scheduler until ctx is canceled.
func (s *BhavcopyService) Start(ctx context.Context) {
	logrus.WithFields(logrus.Fields{
		"provider":       s.provider.Name(),
		"first_attempt":  s.opts.FirstAttempt,
		"retry_until":    s.opts.RetryUntil,
		"retry_interval": s.opts.RetryInterval.String(),
	}).Info("Bhavcopy EOD service started")
	defer logrus.Info("Bhavcopy EOD service stopped")

	schedule := s.firstSchedule(s.opts.Now())
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if schedule.at.IsZero() {
			logrus.Error("Bhavcopy scheduler could not find a trading day")
			return
		}

		now := s.opts.Now()
		if s.opts.Calendar.Date(now).After(s.opts.Calendar.Date(schedule.date)) {
			schedule = s.firstSchedule(now)
			continue
		}

		if wait := schedule.at.Sub(now); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		if err := ctx.Err(); err != nil {
			return
		}

		now = s.opts.Now()
		if s.opts.Calendar.Date(now).After(s.opts.Calendar.Date(schedule.date)) {
			schedule = s.firstSchedule(now)
			continue
		}

		success := s.runAttempt(ctx, schedule.date)
		if err := ctx.Err(); err != nil {
			return
		}
		if success {
			schedule = s.nextDaySchedule(schedule.date)
		} else {
			schedule = s.retrySchedule(schedule.date, now)
		}
	}
}

// firstSchedule returns the first attempt at or after now. On a trading day at
// or after the first-attempt time it runs immediately, including a single
// startup catch-up after the retry window has closed.
func (s *BhavcopyService) firstSchedule(now time.Time) bhavcopySchedule {
	day := s.nextTradingDayOnOrAfter(s.opts.Calendar.Date(now))
	if day.IsZero() {
		return bhavcopySchedule{}
	}

	first := s.attemptTime(day, s.firstMinute)
	if now.Before(first) {
		return bhavcopySchedule{date: day, at: first}
	}
	return bhavcopySchedule{date: day, at: now}
}

// retrySchedule returns the next attempt on the configured grid after a failed
// attempt. A failure at or after the retry window moves to the next trading day.
func (s *BhavcopyService) retrySchedule(date, attemptedAt time.Time) bhavcopySchedule {
	date = s.opts.Calendar.Date(date)
	first := s.attemptTime(date, s.firstMinute)
	last := s.attemptTime(date, s.lastMinute)

	if attemptedAt.Before(first) {
		attemptedAt = first
	}
	slots := attemptedAt.Sub(first) / s.opts.RetryInterval
	next := first.Add((slots + 1) * s.opts.RetryInterval)

	if next.After(last) {
		return s.nextDaySchedule(date)
	}
	return bhavcopySchedule{date: date, at: next}
}

func (s *BhavcopyService) nextDaySchedule(date time.Time) bhavcopySchedule {
	next := s.nextTradingDayAfter(date)
	if next.IsZero() {
		return bhavcopySchedule{}
	}
	return bhavcopySchedule{date: next, at: s.attemptTime(next, s.firstMinute)}
}

func (s *BhavcopyService) nextTradingDayOnOrAfter(day time.Time) time.Time {
	day = s.opts.Calendar.Date(day)
	for i := 0; i < 400; i++ {
		if s.opts.Calendar.IsTradingDay(day) {
			return day
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}
}

func (s *BhavcopyService) nextTradingDayAfter(day time.Time) time.Time {
	return s.nextTradingDayOnOrAfter(s.opts.Calendar.Date(day).AddDate(0, 0, 1))
}

func (s *BhavcopyService) attemptTime(day time.Time, minute int) time.Time {
	local := day.In(s.opts.Calendar.Location())
	return time.Date(
		local.Year(), local.Month(), local.Day(),
		0, 0, 0, 0,
		s.opts.Calendar.Location(),
	).Add(time.Duration(minute) * time.Minute)
}

// runAttempt performs one fetch-and-save pass. It returns true when no retry is
// needed, false when the scheduler should retry on the next grid slot.
func (s *BhavcopyService) runAttempt(ctx context.Context, date time.Time) bool {
	date = s.opts.Calendar.Date(date)
	log := logrus.WithFields(logrus.Fields{
		"provider":    s.provider.Name(),
		"market_date": date.Format("2006-01-02"),
	})

	entries, err := s.store.ListCurrentPrices(ctx)
	if err != nil {
		log.WithError(err).Error("Bhavcopy symbol list failed -- keeping existing prices")
		return false
	}
	if len(entries) == 0 {
		log.Warn("No tracked symbols for bhavcopy -- nothing to update")
		return true
	}

	symbols := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Symbol == "" {
			continue
		}
		symbols = append(symbols, entry.Symbol)
	}
	if len(symbols) == 0 {
		log.Warn("No usable tracked symbols for bhavcopy -- nothing to update")
		return true
	}

	log = log.WithField("symbols", len(symbols))
	started := s.opts.Now()
	quotes, err := s.provider.GetDailyCloses(ctx, date, symbols)
	if err != nil {
		if ctx.Err() != nil {
			log.Info("Bhavcopy fetch canceled")
			return false
		}
		if errors.Is(err, marketdata.ErrNoSession) {
			log.WithError(err).Info("Bhavcopy file not published yet -- will retry within the window")
		} else {
			log.WithError(err).Error("Bhavcopy fetch failed -- keeping existing prices")
		}
		return false
	}

	saved := 0
	precedenceRejected := 0
	historyChanged := 0
	saveErrors := 0
	missing := make([]string, 0)

	for _, symbol := range symbols {
		quote, ok := quotes[symbol]
		if !ok {
			missing = append(missing, symbol)
			continue
		}

		result, err := s.store.SaveQuote(ctx, quote)
		if err != nil {
			saveErrors++
			log.WithError(err).WithField("symbol", symbol).Error("Bhavcopy quote save failed")
			continue
		}
		if result.CurrentAccepted {
			saved++
		} else {
			precedenceRejected++
		}
		if result.HistoryChanged {
			historyChanged++
		}
	}

	fields := logrus.Fields{
		"quotes":              len(quotes),
		"saved":               saved,
		"precedence_rejected": precedenceRejected,
		"history_changed":     historyChanged,
		"missing":             len(missing),
		"save_errors":         saveErrors,
		"duration":            s.opts.Now().Sub(started).String(),
	}
	if len(missing) > 0 {
		fields["missing_symbols"] = summarizeBhavcopySymbols(missing)
	}

	if saveErrors > 0 {
		log.WithFields(fields).Error("Bhavcopy ingestion incomplete -- will retry")
		return false
	}
	if len(quotes) == 0 {
		log.WithFields(fields).Warn("Bhavcopy file contained none of the tracked symbols -- keeping existing prices")
		return true
	}

	log.WithFields(fields).Info("Bhavcopy EOD ingestion completed")
	return true
}

func summarizeBhavcopySymbols(symbols []string) string {
	const maxSymbols = 20
	if len(symbols) <= maxSymbols {
		return strings.Join(symbols, ",")
	}
	return strings.Join(symbols[:maxSymbols], ",") + ",..."
}

func parseBhavcopyClock(label, raw string) (int, error) {
	parsed, err := time.Parse("15:04", raw)
	if err != nil || parsed.Format("15:04") != raw {
		return 0, fmt.Errorf("bhavcopy: invalid %s %q (want HH:MM)", label, raw)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}
