package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
	"github.com/LoganX64/stocky-api/internal/utils"
)

// =======================
// Interfaces
// =======================

type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type Cache interface {
	SetPrice(symbol string, price decimal.Decimal, updatedAt time.Time)
	GetPrice(symbol string) (decimal.Decimal, time.Time, bool)
}

// =======================
// PriceCache
// =======================

type PriceCache struct {
	mu     sync.RWMutex
	prices map[string]CachedPrice
}

type CachedPrice struct {
	Price     decimal.Decimal
	UpdatedAt time.Time
}

func NewPriceCache() *PriceCache {
	return &PriceCache{prices: make(map[string]CachedPrice)}
}

func (pc *PriceCache) SetPrice(symbol string, price decimal.Decimal, updatedAt time.Time) {
	pc.mu.Lock()
	pc.prices[symbol] = CachedPrice{Price: price, UpdatedAt: updatedAt}
	pc.mu.Unlock()
}

func (pc *PriceCache) GetPrice(symbol string) (decimal.Decimal, time.Time, bool) {
	pc.mu.RLock()
	val, ok := pc.prices[symbol]
	pc.mu.RUnlock()
	return val.Price, val.UpdatedAt, ok
}

// =======================
// Options & PriceService
// =======================

type PriceServiceOptions struct {
	Workers         int
	DBMaxConcurrent int
	TickInterval    time.Duration
	MarketHoursOnly bool
	FinalQuoteDelay time.Duration
	Calendar        *marketcal.Calendar
	Freshness       marketcal.FreshnessPolicy
	Now             func() time.Time
}

func defaultOptions() PriceServiceOptions {
	return PriceServiceOptions{
		Workers:         5,
		DBMaxConcurrent: 3,
		TickInterval:    time.Hour,
		MarketHoursOnly: true,
		FinalQuoteDelay: 5 * time.Minute,
		Calendar:        marketcal.Default(),
		Freshness:       marketcal.DefaultFreshnessPolicy(marketcal.Default()),
		Now:             time.Now,
	}
}

func resolveOptions(in *PriceServiceOptions) PriceServiceOptions {
	o := defaultOptions()
	if in == nil {
		return o
	}
	if in.Workers > 0 {
		o.Workers = in.Workers
	}
	if in.DBMaxConcurrent > 0 {
		o.DBMaxConcurrent = in.DBMaxConcurrent
	}
	if in.TickInterval > 0 {
		o.TickInterval = in.TickInterval
	}
	o.MarketHoursOnly = in.MarketHoursOnly
	if in.FinalQuoteDelay > 0 {
		o.FinalQuoteDelay = in.FinalQuoteDelay
	}
	if in.Calendar != nil {
		o.Calendar = in.Calendar
	}
	if in.Now != nil {
		o.Now = in.Now
	}

	o.Freshness = in.Freshness
	if o.Freshness.Calendar == nil {
		o.Freshness.Calendar = o.Calendar
	}
	if o.Freshness.MaxStale <= 0 {
		o.Freshness.MaxStale = marketcal.DefaultMaxStale
	}
	if o.Freshness.CloseGrace <= 0 {
		o.Freshness.CloseGrace = marketcal.DefaultCloseGrace
	}
	if o.Freshness.FutureSkew <= 0 {
		o.Freshness.FutureSkew = marketcal.DefaultFutureSkew
	}

	if o.DBMaxConcurrent > o.Workers {
		o.DBMaxConcurrent = o.Workers
	}
	return o
}

type PriceService struct {
	store    PriceStore
	cache    Cache
	provider marketdata.QuoteProvider
	opts     PriceServiceOptions

	dbSem chan struct{}

	running int32
	started int32
}

func NewPriceService(store PriceStore, cache Cache, provider marketdata.QuoteProvider, opts *PriceServiceOptions) *PriceService {
	o := resolveOptions(opts)
	return &PriceService{
		store:    store,
		cache:    cache,
		provider: provider,
		opts:     o,
		dbSem:    make(chan struct{}, o.DBMaxConcurrent),
	}
}

// =======================
// Scheduler
// =======================

func (s *PriceService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("PriceService.Start called more than once -- ignoring duplicate")
		return
	}

	latest, hasLatest := s.initializeCache(ctx)
	logrus.Info("Price updater started")

	if s.shouldCatchUp(s.opts.Now(), latest, hasLatest) {
		s.triggerUpdate(ctx)
	}

	for {
		now := s.opts.Now()
		next := s.nextScheduledRun(now)
		if next.IsZero() || !next.After(now) {
			next = now.Add(s.opts.TickInterval)
		}

		timer := time.NewTimer(next.Sub(now))
		select {
		case <-timer.C:
			s.triggerUpdate(ctx)
		case <-ctx.Done():
			timer.Stop()
			logrus.Info("Price updater stopping -- waiting for in-flight cycle")
			for !atomic.CompareAndSwapInt32(&s.running, 0, 0) {
				time.Sleep(50 * time.Millisecond)
			}
			logrus.Info("Price updater stopped cleanly")
			return
		}
	}
}

func (s *PriceService) nextScheduledRun(now time.Time) time.Time {
	if !s.opts.MarketHoursOnly {
		return now.Add(s.opts.TickInterval)
	}

	calendar := s.opts.Calendar
	if !calendar.IsTradingDay(now) {
		return calendar.NextSessionOpen(now)
	}

	open := calendar.SessionOpen(now)
	close := calendar.SessionClose(now)
	final := close.Add(s.opts.FinalQuoteDelay)

	switch {
	case now.Before(open):
		return open
	case now.Before(close):
		elapsed := now.Sub(open)
		ticks := elapsed/s.opts.TickInterval + 1
		next := open.Add(ticks * s.opts.TickInterval)
		if !next.Before(close) {
			return final
		}
		return next
	case now.Before(final):
		return final
	default:
		return calendar.NextSessionOpen(now)
	}
}

func (s *PriceService) shouldCatchUp(now time.Time, latest CurrentPrice, hasLatest bool) bool {
	if !s.opts.MarketHoursOnly {
		return true
	}

	calendar := s.opts.Calendar
	if !calendar.IsTradingDay(now) {
		return false
	}
	if now.Before(calendar.SessionOpen(now)) {
		return false
	}
	if !hasLatest || latest.AsOf.IsZero() {
		return true
	}

	return s.opts.Freshness.ClassifySource(latest.Source, latest.AsOf, now) != marketcal.FreshnessFresh
}

func (s *PriceService) triggerUpdate(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		logrus.Warn("Price update cycle still running -- skipping this tick")
		return
	}

	go func() {
		defer atomic.StoreInt32(&s.running, 0)

		defer func() {
			if r := recover(); r != nil {
				logrus.WithField("panic", r).
					WithField("stack", string(debug.Stack())).
					Error("Panic recovered in price update cycle")
			}
		}()
		s.updatePrices(ctx)
	}()
}

// =======================
// Cache initialization
// =======================

func (s *PriceService) initializeCache(ctx context.Context) (CurrentPrice, bool) {
	entries, err := s.store.ListCurrentPrices(ctx)
	if err != nil {
		logrus.WithError(err).Error("Cache initialisation failed")
		return CurrentPrice{}, false
	}

	var latest CurrentPrice
	for _, e := range entries {
		s.cache.SetPrice(e.Symbol, e.Price, e.AsOf)
		if !e.AsOf.IsZero() && (latest.AsOf.IsZero() || e.AsOf.After(latest.AsOf)) {
			latest = e
		}
	}
	logrus.Infof("Cache seeded with %d symbols", len(entries))

	return latest, !latest.AsOf.IsZero()
}

// =======================
// Cycle accounting
// =======================

// priceCycleSequence gives every scheduler cycle a stable ID that can be used
// to correlate worker, provider, and database log lines.
var priceCycleSequence atomic.Uint64

func newPriceCycleID(now time.Time) string {
	return fmt.Sprintf("%s-%d", now.UTC().Format("20060102T150405.000Z"), priceCycleSequence.Add(1))
}

type cycleCounts struct {
	Fetched        int
	Saved          int
	Ignored        int
	Failed         int
	HistoryChanged int
}

const (
	cycleFetched = iota
	cycleSaved
	cycleIgnored
	cycleFailed
	cycleHistoryChanged
)

// cycleStats is shared by all workers in one cycle. A nil receiver is allowed
// so direct processJob callers (tests) do not need to provide accounting.
type cycleStats struct {
	mu     sync.Mutex
	counts cycleCounts
}

func (c *cycleStats) record(outcome int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	switch outcome {
	case cycleFetched:
		c.counts.Fetched++
	case cycleSaved:
		c.counts.Saved++
	case cycleIgnored:
		c.counts.Ignored++
	case cycleFailed:
		c.counts.Failed++
	case cycleHistoryChanged:
		c.counts.HistoryChanged++
	}
}

func (c *cycleStats) snapshot() cycleCounts {
	if c == nil {
		return cycleCounts{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts
}

// =======================
// Worker pipeline
// =======================

type PriceJob struct {
	Symbol    string
	OldPrice  decimal.Decimal
	UpdatedAt time.Time
}

func (s *PriceService) updatePrices(ctx context.Context) {
	started := s.opts.Now()
	cycleID := newPriceCycleID(started)
	log := logrus.WithFields(logrus.Fields{
		"cycle_id": cycleID,
		"provider": s.provider.Name(),
	})

	entries, err := s.store.ListCurrentPrices(ctx)
	if err != nil {
		log.WithError(err).WithField("event", "cycle_failed").
			Error("Failed to fetch symbols -- skipping cycle")
		return
	}
	if len(entries) == 0 {
		log.WithField("event", "cycle_skipped").Warn("No symbols found -- nothing to update")
		return
	}

	log.WithFields(logrus.Fields{
		"event":   "cycle_started",
		"symbols": len(entries),
		"workers": s.opts.Workers,
	}).Info("Price update cycle started")

	bufSize := len(entries)
	if bufSize > 100 {
		bufSize = 100
	}
	jobs := make(chan PriceJob, bufSize)

	stats := &cycleStats{}
	var wg sync.WaitGroup
	for i := 0; i < s.opts.Workers; i++ {
		wg.Add(1)
		go s.worker(ctx, jobs, &wg, cycleID, stats)
	}

	go func() {
		defer close(jobs)
		for _, e := range entries {
			job := PriceJob{
				Symbol:    e.Symbol,
				OldPrice:  e.Price,
				UpdatedAt: e.AsOf,
			}
			select {
			case jobs <- job:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()

	counts := stats.snapshot()
	log.WithFields(logrus.Fields{
		"event":           "cycle_completed",
		"symbols":         len(entries),
		"fetched":         counts.Fetched,
		"saved":           counts.Saved,
		"ignored":         counts.Ignored,
		"failed":          counts.Failed,
		"history_changed": counts.HistoryChanged,
		"duration_ms":     s.opts.Now().Sub(started).Milliseconds(),
	}).Info("Price update cycle completed")
}

func (s *PriceService) worker(ctx context.Context, jobs <-chan PriceJob, wg *sync.WaitGroup, cycleID string, stats *cycleStats) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			logrus.WithField("cycle_id", cycleID).
				WithField("panic", r).
				WithField("stack", string(debug.Stack())).
				Error("Panic recovered in price worker -- worker exiting cleanly")
		}
	}()

	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}
			s.processJobWithCycle(ctx, job, cycleID, stats)
		case <-ctx.Done():
			return
		}
	}
}

// =======================
// Core logic
// =======================

func (s *PriceService) processJob(ctx context.Context, job PriceJob) {
	s.processJobWithCycle(ctx, job, "", nil)
}

func (s *PriceService) processJobWithCycle(ctx context.Context, job PriceJob, cycleID string, stats *cycleStats) {
	symbol := utils.NormalizeStockSymbol(job.Symbol)
	fields := logrus.Fields{"symbol": symbol}
	if cycleID != "" {
		fields["cycle_id"] = cycleID
	}
	log := logrus.WithFields(fields)

	quote, err := s.provider.GetQuote(ctx, symbol)
	if err != nil {
		stats.record(cycleFailed)
		log.WithError(err).
			WithFields(logrus.Fields{
				"event":     "fetch_failed",
				"provider":  s.provider.Name(),
				"old_price": job.OldPrice.String(),
			}).
			Warn("Price fetch failed - keeping last known price")
		return
	}
	stats.record(cycleFetched)

	if quote.Symbol != symbol {
		stats.record(cycleFailed)
		log.WithFields(logrus.Fields{
			"event":        "quote_rejected",
			"reason":       "symbol_mismatch",
			"provider":     s.provider.Name(),
			"quote_symbol": quote.Symbol,
		}).Error("Provider returned a quote for an unexpected symbol -- discarding")
		return
	}

	select {
	case s.dbSem <- struct{}{}:
		defer func() { <-s.dbSem }()
	case <-ctx.Done():
		return
	}

	result, err := s.store.SaveQuote(ctx, quote)
	if err != nil {
		stats.record(cycleFailed)
		log.WithError(err).
			WithFields(logrus.Fields{
				"event":           "quote_save_failed",
				"provider":        quote.Source,
				"provider_symbol": quote.ProviderSymbol,
				"exchange":        quote.Exchange,
				"currency":        quote.Currency,
				"quote_as_of":     quote.AsOf,
				"fetched_at":      quote.FetchedAt,
			}).
			Error("Price save failed -- cache not updated")
		return
	}
	if !result.CurrentAccepted {
		stats.record(cycleIgnored)
		log.WithFields(logrus.Fields{
			"event":           "quote_ignored_older",
			"db_result":       "rejected",
			"provider":        quote.Source,
			"provider_symbol": quote.ProviderSymbol,
			"exchange":        quote.Exchange,
			"currency":        quote.Currency,
			"price":           quote.Price.String(),
			"quote_as_of":     quote.AsOf,
			"fetched_at":      quote.FetchedAt,
			"history_changed": result.HistoryChanged,
		}).Info("Quote ignored because a newer or higher-precedence price already exists")
		return
	}

	stats.record(cycleSaved)
	if result.HistoryChanged {
		stats.record(cycleHistoryChanged)
	}

	s.cache.SetPrice(symbol, quote.Price, quote.AsOf)

	if quote.Price.Equal(job.OldPrice) {
		log.WithFields(logrus.Fields{
			"event":           "quote_saved",
			"db_result":       "unchanged",
			"provider":        quote.Source,
			"provider_symbol": quote.ProviderSymbol,
			"exchange":        quote.Exchange,
			"currency":        quote.Currency,
			"price":           quote.Price.String(),
			"quote_as_of":     quote.AsOf,
			"fetched_at":      quote.FetchedAt,
			"history_changed": result.HistoryChanged,
		}).Debug("Price unchanged, quote timestamp and provenance refreshed")
		return
	}

	log.WithFields(logrus.Fields{
		"event":           "quote_saved",
		"db_result":       "updated",
		"provider":        quote.Source,
		"provider_symbol": quote.ProviderSymbol,
		"exchange":        quote.Exchange,
		"currency":        quote.Currency,
		"old_price":       job.OldPrice.String(),
		"new_price":       quote.Price.String(),
		"change":          quote.Price.Sub(job.OldPrice).String(),
		"quote_as_of":     quote.AsOf,
		"fetched_at":      quote.FetchedAt,
		"history_changed": result.HistoryChanged,
	}).Info("Price updated successfully")
}
