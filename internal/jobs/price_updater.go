package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/sirupsen/logrus"
)

//
// =======================
// Interfaces
// =======================
//

type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type Cache interface {
	SetPrice(symbol string, price float64, updatedAt time.Time)
	GetPrice(symbol string) (float64, time.Time, bool)
}

type PriceFetcher interface {
	GetLatestPrice(symbol string, lastPrice float64) (float64, error)
}

//
// =======================
// PriceCache
// =======================
//

type PriceCache struct {
	mu     sync.RWMutex
	prices map[string]CachedPrice
}

type CachedPrice struct {
	Price     float64
	UpdatedAt time.Time
}

func NewPriceCache() *PriceCache {
	return &PriceCache{prices: make(map[string]CachedPrice)}
}

func (pc *PriceCache) SetPrice(symbol string, price float64, updatedAt time.Time) {
	pc.mu.Lock()
	pc.prices[symbol] = CachedPrice{Price: price, UpdatedAt: updatedAt}
	pc.mu.Unlock()
}

func (pc *PriceCache) GetPrice(symbol string) (float64, time.Time, bool) {
	pc.mu.RLock()
	val, ok := pc.prices[symbol]
	pc.mu.RUnlock()
	return val.Price, val.UpdatedAt, ok
}

//
// =======================
// RandomPriceFetcher
// =======================
//

type RandomPriceFetcher struct {
	FallbackFactor float64
}

func NewRandomPriceFetcher() *RandomPriceFetcher {
	return &RandomPriceFetcher{FallbackFactor: 0.01}
}

func (r *RandomPriceFetcher) effectiveFallbackFactor() float64 {
	if r.FallbackFactor == 0 {
		return 0.01
	}
	return r.FallbackFactor
}

func (r *RandomPriceFetcher) GetLatestPrice(symbol string, lastPrice float64) (float64, error) {
	if rand.Float64() < 0.1 {
		f := r.effectiveFallbackFactor()
		dampened := utils.RoundAmount(lastPrice * (1.0 - f + rand.Float64()*(2*f)))
		return dampened, fmt.Errorf("price fetch failed for %s: using dampened fallback", symbol)
	}
	return utils.RoundAmount(lastPrice * (0.95 + rand.Float64()*0.10)), nil
}

//
// =======================
// Options
// =======================
//

type PriceServiceOptions struct {
	MaxStale time.Duration

	Workers int

	DBMaxConcurrent int

	TickInterval time.Duration

	Now func() time.Time
}

func defaultOptions() PriceServiceOptions {
	return PriceServiceOptions{
		MaxStale:        120 * time.Minute,
		Workers:         5,
		DBMaxConcurrent: 3,
		TickInterval:    time.Hour,
		Now:             time.Now,
	}
}

func resolveOptions(in *PriceServiceOptions) PriceServiceOptions {
	o := defaultOptions()
	if in == nil {
		return o
	}
	if in.MaxStale > 0 {
		o.MaxStale = in.MaxStale
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
	if in.Now != nil {
		o.Now = in.Now
	}

	if o.DBMaxConcurrent > o.Workers {
		o.DBMaxConcurrent = o.Workers
	}
	return o
}

//
// =======================
// PriceService
// =======================
//

type PriceService struct {
	db      DB
	cache   Cache
	fetcher PriceFetcher
	opts    PriceServiceOptions

	dbSem chan struct{}

	running int32
	started int32
}

func NewPriceService(db DB, cache Cache, fetcher PriceFetcher, opts *PriceServiceOptions) *PriceService {
	o := resolveOptions(opts)
	return &PriceService{
		db:      db,
		cache:   cache,
		fetcher: fetcher,
		opts:    o,
		dbSem:   make(chan struct{}, o.DBMaxConcurrent),
	}
}

//
// =======================
// Scheduler
// =======================
//

func (s *PriceService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("PriceService.Start called more than once -- ignoring duplicate")
		return
	}

	s.initializeCache(ctx)

	ticker := time.NewTicker(s.opts.TickInterval)
	defer ticker.Stop()

	logrus.Info("Price updater started")

	s.triggerUpdate(ctx)

	for {
		select {
		case <-ticker.C:
			s.triggerUpdate(ctx)
		case <-ctx.Done():
			logrus.Info("Price updater stopping -- waiting for in-flight cycle")
			for !atomic.CompareAndSwapInt32(&s.running, 0, 0) {
				time.Sleep(50 * time.Millisecond)
			}
			logrus.Info("Price updater stopped cleanly")
			return
		}
	}
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

//
// =======================
// Cache initialisation
// =======================
//

func (s *PriceService) initializeCache(ctx context.Context) {
	entries, err := s.querySymbolPrices(ctx)
	if err != nil {
		logrus.WithError(err).Error("Cache initialisation failed")
		return
	}
	for _, e := range entries {
		s.cache.SetPrice(e.Symbol, e.OldPrice, e.UpdatedAt)
	}
	logrus.Infof("Cache seeded with %d symbols", len(entries))
}

//
// =======================
// Worker pipeline
// =======================
//

type PriceJob struct {
	Symbol    string
	OldPrice  float64
	UpdatedAt time.Time
}

func (s *PriceService) updatePrices(ctx context.Context) {
	entries, err := s.querySymbolPrices(ctx)
	if err != nil {
		logrus.WithError(err).Error("Failed to fetch symbols -- skipping cycle")
		return
	}
	if len(entries) == 0 {
		logrus.Warn("No symbols found -- nothing to update")
		return
	}

	bufSize := len(entries)
	if bufSize > 100 {
		bufSize = 100
	}
	jobs := make(chan PriceJob, bufSize)

	var wg sync.WaitGroup
	for i := 0; i < s.opts.Workers; i++ {
		wg.Add(1)
		go s.worker(ctx, jobs, &wg)
	}

	go func() {
		defer close(jobs)
		for _, e := range entries {
			select {
			case jobs <- e:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
}

func (s *PriceService) worker(ctx context.Context, jobs <-chan PriceJob, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			logrus.WithField("panic", r).
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
			s.processJob(ctx, job)
		case <-ctx.Done():
			return
		}
	}
}

//
// =======================
// Core logic
// =======================
//

func (s *PriceService) processJob(ctx context.Context, job PriceJob) {
	log := logrus.WithField("symbol", job.Symbol)

	newPrice, fetchErr := s.fetcher.GetLatestPrice(job.Symbol, job.OldPrice)
	if fetchErr != nil {
		log.WithError(fetchErr).Warn("Price fetch degraded; proceeding with fetcher fallback price")
	}

	if newPrice == job.OldPrice {
		return
	}

	select {
	case s.dbSem <- struct{}{}:
		defer func() { <-s.dbSem }()
	case <-ctx.Done():
		return
	}

	if err := s.updatePrice(ctx, job.Symbol, newPrice); err != nil {
		log.WithError(err).Error("DB price update failed -- cache not updated")
		return
	}

	s.cache.SetPrice(job.Symbol, newPrice, s.opts.Now())

	if err := s.insertHistory(ctx, job.Symbol, newPrice); err != nil {
		log.WithError(err).Error("History insert failed")
	}
}

//
// =======================
// DB helpers
// =======================
//

func (s *PriceService) querySymbolPrices(ctx context.Context) ([]PriceJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT stock_symbol, price, updated_at FROM stock_prices`)
	if err != nil {
		return nil, fmt.Errorf("querySymbolPrices: %w", err)
	}
	defer rows.Close()

	var entries []PriceJob
	for rows.Next() {
		var e PriceJob
		if err := rows.Scan(&e.Symbol, &e.OldPrice, &e.UpdatedAt); err != nil {
			logrus.WithError(err).Warn("Skipping malformed row in stock_prices")
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querySymbolPrices row iteration: %w", err)
	}
	return entries, nil
}

func (s *PriceService) updatePrice(ctx context.Context, symbol string, price float64) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		_, lastErr = s.db.ExecContext(ctx,
			`UPDATE stock_prices SET price=$1, updated_at=NOW() WHERE stock_symbol=$2`,
			price, symbol,
		)
		if lastErr == nil {
			return nil
		}

		logrus.WithField("symbol", symbol).
			WithField("attempt", attempt+1).
			WithError(lastErr).Warn("updatePrice retry")

		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-timer.C:

		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("updatePrice %s: all 3 attempts failed: %w", symbol, lastErr)
}

func (s *PriceService) insertHistory(ctx context.Context, symbol string, price float64) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		_, lastErr = s.db.ExecContext(ctx, `
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES ($1, $2, CURRENT_DATE)
			ON CONFLICT (stock_symbol, date) DO UPDATE
			SET price = EXCLUDED.price
		`, symbol, price)
		if lastErr == nil {
			return nil
		}

		logrus.WithField("symbol", symbol).
			WithField("attempt", attempt+1).
			WithError(lastErr).Warn("insertHistory retry")

		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("insertHistory %s: all 3 attempts failed: %w", symbol, lastErr)
}
