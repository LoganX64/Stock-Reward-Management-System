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

// Thread-safe cache (same as yours)
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

// Default price fetcher (your existing logic)
type RandomPriceFetcher struct{}

func (r *RandomPriceFetcher) GetLatestPrice(symbol string, lastPrice float64) (float64, error) {
	if rand.Float64() < 0.1 {
		return 0, fmt.Errorf("price fetch failed")
	}
	factor := 0.95 + rand.Float64()*0.10
	return utils.RoundAmount(lastPrice * factor), nil
}

//
// =======================
// Service Layer
// =======================
//

type PriceService struct {
	db       DB
	cache    Cache
	fetcher  PriceFetcher
	now      func() time.Time
	running  int32
	maxStale time.Duration
	workers  int
}

func NewPriceService(db DB, cache Cache, fetcher PriceFetcher) *PriceService {
	return &PriceService{
		db:       db,
		cache:    cache,
		fetcher:  fetcher,
		now:      time.Now,
		maxStale: 120 * time.Minute,
		workers:  5,
	}
}

//
// =======================
// Scheduler
// =======================
//

func (s *PriceService) Start(ctx context.Context) {
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT stock_symbol, price, updated_at 
		FROM stock_prices
	`)
	if err != nil {
		logrus.WithError(err).Error("Cache init failed")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var symbol string
		var price float64
		var updatedAt time.Time

		if err := rows.Scan(&symbol, &price, &updatedAt); err != nil {
			logrus.WithError(err).Warn("Skipping invalid row")
			continue
		}
		s.cache.SetPrice(symbol, price, updatedAt)
	}

	if err := rows.Err(); err != nil {
		logrus.WithError(err).Error("Row iteration error")
	}
}

//
// =======================
// Worker Pipeline
// =======================
//

type PriceJob struct {
	Symbol   string
	OldPrice float64
}

func (s *PriceService) updatePrices(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT stock_symbol, price FROM stock_prices`)
	if err != nil {
		logrus.WithError(err).Error("Fetch failed")
		return
	}
	defer rows.Close()

	jobs := make(chan PriceJob, 100)
	var wg sync.WaitGroup

	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go s.worker(ctx, jobs, &wg)
	}

	for rows.Next() {
		var symbol string
		var price float64

		if err := rows.Scan(&symbol, &price); err != nil {
			continue
		}

		select {
		case jobs <- PriceJob{Symbol: symbol, OldPrice: price}:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}

	if err := rows.Err(); err != nil {
		logrus.WithError(err).Error("Row iteration error")
	}

	close(jobs)
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
// Core Logic (clean & testable)
// =======================
//

func (s *PriceService) processJob(ctx context.Context, job PriceJob) {
	newPrice, err := s.fetcher.GetLatestPrice(job.Symbol, job.OldPrice)
	if err != nil {
		factor := 0.99 + rand.Float64()*0.02
		newPrice = utils.RoundAmount(job.OldPrice * factor)
	}

	if newPrice == job.OldPrice {
		return
	}

	if err := s.updateWithFallback(ctx, job.Symbol, newPrice); err != nil {
		return
	}

	s.cache.SetPrice(job.Symbol, newPrice, s.now())

	if err := s.insertHistory(ctx, job.Symbol, newPrice); err != nil {
		logrus.WithError(err).Error("History insert failed")
	}
}

func (s *PriceService) updateWithFallback(ctx context.Context, symbol string, price float64) error {
	if err := s.updatePrice(ctx, symbol, price); err == nil {
		return nil
	}

	cached, t, ok := s.cache.GetPrice(symbol)
	if !ok || time.Since(t) > s.maxStale {
		return fmt.Errorf("no valid fallback")
	}

	logrus.Infof("Using cached price for %s", symbol)
	return s.updatePrice(ctx, symbol, cached)
}

//
// =======================
// DB Operations
// =======================
//

func (s *PriceService) updatePrice(ctx context.Context, symbol string, price float64) error {
	for i := 0; i < 3; i++ {
		_, err := s.db.ExecContext(ctx,
			`UPDATE stock_prices SET price=$1, updated_at=NOW() WHERE stock_symbol=$2`,
			price, symbol,
		)
		if err == nil {
			return nil
		}

		select {
		case <-time.After(time.Duration(i+1) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("update failed")
}

func (s *PriceService) insertHistory(ctx context.Context, symbol string, price float64) error {
	for i := 0; i < 3; i++ {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES ($1, $2, CURRENT_DATE)
			ON CONFLICT (stock_symbol, date) DO UPDATE 
			SET price = EXCLUDED.price
		`, symbol, price)

		if err == nil {
			return nil
		}

		select {
		case <-time.After(time.Duration(i+1) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("insert failed")
}
