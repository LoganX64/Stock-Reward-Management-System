package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/sirupsen/logrus"
)

//
// =======================
// Interfaces (for testing & extensibility)
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
// Default Implementations
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
	return &PriceCache{
		prices: make(map[string]CachedPrice),
	}
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

type RandomPriceFetcher struct {
	FallbackFactor float64
}

func NewRandomPriceFetcher() *RandomPriceFetcher {
	return &RandomPriceFetcher{FallbackFactor: 0.01}
}

func (r *RandomPriceFetcher) GetLatestPrice(symbol string, lastPrice float64) (float64, error) {
	if rand.Float64() < 0.1 {
		lo := 1.0 - r.FallbackFactor
		hi := 1.0 + r.FallbackFactor
		dampened := utils.RoundAmount(lastPrice * (lo + rand.Float64()*(hi-lo)))
		return dampened, fmt.Errorf("price fetch failed for %s: using dampened fallback", symbol)
	}
	factor := 0.95 + rand.Float64()*0.10
	return utils.RoundAmount(lastPrice * factor), nil
}

//
// =======================
// Service Options
// =======================
//

type PriceServiceOptions struct {
	MaxStale time.Duration
	Workers  int
	Now      func() time.Time
}

func defaultOptions() PriceServiceOptions {
	return PriceServiceOptions{
		MaxStale: 120 * time.Minute,
		Workers:  5,
		Now:      time.Now,
	}
}

//
// =======================
// Service Layer
// =======================
//

type PriceService struct {
	db      DB
	cache   Cache
	fetcher PriceFetcher
	opts    PriceServiceOptions
	running int32
	started int32
}

func NewPriceService(db DB, cache Cache, fetcher PriceFetcher, opts *PriceServiceOptions) *PriceService {
	o := defaultOptions()
	if opts != nil {
		if opts.MaxStale > 0 {
			o.MaxStale = opts.MaxStale
		}
		if opts.Workers > 0 {
			o.Workers = opts.Workers
		}
		if opts.Now != nil {
			o.Now = opts.Now
		}
	}
	return &PriceService{
		db:      db,
		cache:   cache,
		fetcher: fetcher,
		opts:    o,
	}
}

//
// =======================
// Scheduler
// =======================
//

func (s *PriceService) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		logrus.Warn("PriceService.Start called more than once — ignoring")
		return
	}

	s.initializeCache(ctx)

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	logrus.Info("Price updater started")

	s.triggerUpdate(ctx)

	for {
		select {
		case <-ticker.C:
			s.triggerUpdate(ctx)
		case <-ctx.Done():
			logrus.Info("Stopping price updater...")
			return
		}
	}
}

func (s *PriceService) triggerUpdate(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		logrus.Warn("Previous update still running, skipping...")
		return
	}

	go func() {
		defer atomic.StoreInt32(&s.running, 0)
		s.updatePrices(ctx)
	}()
}

//
// =======================
// Initialization
// =======================
//

func (s *PriceService) initializeCache(ctx context.Context) {
	entries, err := s.querySymbolPrices(ctx)
	if err != nil {
		logrus.WithError(err).Error("Cache init failed")
		return
	}

	for _, e := range entries {
		s.cache.SetPrice(e.Symbol, e.OldPrice, e.UpdatedAt)
	}
}

//
// =======================
// Worker Pipeline
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
		logrus.WithError(err).Error("Failed to fetch symbols for price update")
		return
	}

	jobs := make(chan PriceJob, 100)
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
// Core Logic
// =======================
//

func (s *PriceService) processJob(ctx context.Context, job PriceJob) {
	log := logrus.WithField("symbol", job.Symbol)

	newPrice, fetchErr := s.fetcher.GetLatestPrice(job.Symbol, job.OldPrice)
	if fetchErr != nil {

		log.WithError(fetchErr).Warn("Price fetch degraded; using fetcher fallback")
	}

	if newPrice == job.OldPrice {
		return
	}

	if err := s.updatePrice(ctx, job.Symbol, newPrice); err != nil {
		log.WithError(err).Error("Failed to update price in DB")
		return
	}

	s.cache.SetPrice(job.Symbol, newPrice, s.opts.Now())

	if err := s.insertHistory(ctx, job.Symbol, newPrice); err != nil {
		log.WithError(err).Error("History insert failed")
	}
}

//
// =======================
// DB Operations
// =======================
//

func (s *PriceService) querySymbolPrices(ctx context.Context) ([]PriceJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT stock_symbol, price, updated_at
		FROM stock_prices
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []PriceJob
	for rows.Next() {
		var e PriceJob
		if err := rows.Scan(&e.Symbol, &e.OldPrice, &e.UpdatedAt); err != nil {
			logrus.WithError(err).Warn("Skipping invalid row during symbol query")
			continue
		}
		entries = append(entries, e)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return entries, nil
}

func (s *PriceService) updatePrice(ctx context.Context, symbol string, price float64) error {
	var lastErr error
	for i := 0; i < 3; i++ {
		_, lastErr = s.db.ExecContext(ctx,
			`UPDATE stock_prices SET price=$1, updated_at=NOW() WHERE stock_symbol=$2`,
			price, symbol,
		)
		if lastErr == nil {
			return nil
		}

		timer := time.NewTimer(time.Duration(i+1) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("update price for %s failed after 3 attempts: %w", symbol, lastErr)
}

func (s *PriceService) insertHistory(ctx context.Context, symbol string, price float64) error {
	var lastErr error
	for i := 0; i < 3; i++ {
		_, lastErr = s.db.ExecContext(ctx, `
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES ($1, $2, CURRENT_DATE)
			ON CONFLICT (stock_symbol, date) DO UPDATE
			SET price = EXCLUDED.price
		`, symbol, price)

		if lastErr == nil {
			return nil
		}

		timer := time.NewTimer(time.Duration(i+1) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("insert history for %s failed after 3 attempts: %w", symbol, lastErr)
}
