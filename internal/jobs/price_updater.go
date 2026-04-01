package jobs

import (
	"context"
	"database/sql"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/sirupsen/logrus"
)

type PriceCache struct {
	mu     sync.RWMutex
	prices map[string]CachedPrice
}

type CachedPrice struct {
	Price     float64
	UpdatedAt time.Time
}

type PriceJob struct {
	Symbol   string
	OldPrice float64
}

var (
	priceCache           = &PriceCache{prices: make(map[string]CachedPrice)}
	maxPriceStaleMinutes = 120
	isRunning            int32
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// =======================
//
//	Scheduler
//
// =======================
func StartPriceUpdater(ctx context.Context, db *sql.DB) {
	initializePriceCache(ctx, db)

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	logrus.Info("Price updater started (native scheduler)")

	// Run immediately once
	triggerUpdate(ctx, db)

	for {
		select {
		case <-ticker.C:
			triggerUpdate(ctx, db)

		case <-ctx.Done():
			logrus.Info("Stopping price updater...")
			return
		}
	}
}

func triggerUpdate(ctx context.Context, db *sql.DB) {
	if !atomic.CompareAndSwapInt32(&isRunning, 0, 1) {
		logrus.Warn("Previous update still running, skipping...")
		return
	}

	go func() {
		defer atomic.StoreInt32(&isRunning, 0)
		updatePrices(ctx, db)
	}()
}

// =======================
//
//	Cache Initialization
//
// =======================
func initializePriceCache(ctx context.Context, db *sql.DB) {
	rows, err := db.QueryContext(ctx, `
		SELECT stock_symbol, price, updated_at 
		FROM stock_prices
	`)
	if err != nil {
		logrus.WithError(err).Error("Failed to initialize price cache")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var symbol string
		var price float64
		var updatedAt time.Time

		if err := rows.Scan(&symbol, &price, &updatedAt); err != nil {
			continue
		}
		priceCache.SetPrice(symbol, price, updatedAt)
	}
}

// =======================
//
//	Cache Methods
//
// =======================
func (pc *PriceCache) SetPrice(symbol string, price float64, updatedAt time.Time) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.prices[symbol] = CachedPrice{
		Price:     price,
		UpdatedAt: updatedAt,
	}
}

func (pc *PriceCache) GetPrice(symbol string) (float64, time.Time, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	cached, ok := pc.prices[symbol]
	return cached.Price, cached.UpdatedAt, ok
}

// =======================
//
//	Producer + Workers
//
// =======================
func updatePrices(ctx context.Context, db *sql.DB) {
	logrus.Info("Updating stock prices...")

	rows, err := db.QueryContext(ctx, `SELECT stock_symbol, price FROM stock_prices`)
	if err != nil {
		logrus.WithError(err).Error("Failed to fetch stock prices")
		return
	}
	defer rows.Close()

	jobs := make(chan PriceJob, 100)
	var wg sync.WaitGroup

	numWorkers := 5

	// Start workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go priceWorker(ctx, db, jobs, &wg)
	}

	// Produce jobs
	for rows.Next() {
		var symbol string
		var oldPrice float64

		if err := rows.Scan(&symbol, &oldPrice); err != nil {
			logrus.WithError(err).Warn("Failed to scan stock price")
			continue
		}

		select {
		case jobs <- PriceJob{Symbol: symbol, OldPrice: oldPrice}:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}

	close(jobs)
	wg.Wait()

	logrus.Info("Stock price update completed")
}

// =======================
//
//	Worker
//
// =======================
func priceWorker(ctx context.Context, db *sql.DB, jobs <-chan PriceJob, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}

			processJob(ctx, db, job)

		case <-ctx.Done():
			return
		}
	}
}

func processJob(ctx context.Context, db *sql.DB, job PriceJob) {
	symbol := job.Symbol
	oldPrice := job.OldPrice

	newPrice, err := getLatestPrice(symbol, oldPrice)
	if err != nil {
		logrus.WithError(err).Warnf("Fallback price for %s", symbol)
		factor := 0.99 + rand.Float64()*0.02
		newPrice = utils.RoundAmount(oldPrice * factor)
	}

	updateSuccess := false

	if err := safeUpdatePrice(ctx, db, symbol, newPrice); err != nil {

		if cachedPrice, cachedTime, ok := priceCache.GetPrice(symbol); ok {
			staleness := time.Since(cachedTime)

			if staleness.Minutes() < float64(maxPriceStaleMinutes) {
				logrus.Infof("Using cached price for %s", symbol)

				if err := safeUpdatePrice(ctx, db, symbol, cachedPrice); err == nil {
					newPrice = cachedPrice
					updateSuccess = true
				}
			}
		}

		if !updateSuccess {
			return
		}
	} else {
		updateSuccess = true
	}

	if updateSuccess {
		priceCache.SetPrice(symbol, newPrice, time.Now())
	}

	if err := safeInsertPriceHistory(ctx, db, symbol, newPrice); err != nil {
		logrus.WithError(err).Errorf("History insert failed for %s", symbol)
		return
	}

	logrus.Infof("Updated %s: %.2f -> %.2f", symbol, oldPrice, newPrice)
}

// =======================
//
//	Price Logic
//
// =======================
func getLatestPrice(symbol string, lastPrice float64) (float64, error) {
	if rand.Float64() < 0.1 {
		return 0, sql.ErrConnDone
	}

	factor := 0.95 + rand.Float64()*0.10
	return utils.RoundAmount(lastPrice * factor), nil
}

// =======================
//
//	DB Safe Ops
//
// =======================
func safeUpdatePrice(ctx context.Context, db *sql.DB, symbol string, newPrice float64) error {
	const maxRetries = 3

	for i := 0; i < maxRetries; i++ {
		_, err := db.ExecContext(ctx,
			`UPDATE stock_prices SET price=$1, updated_at=NOW() WHERE stock_symbol=$2`,
			newPrice, symbol,
		)
		if err == nil {
			return nil
		}

		logrus.WithError(err).Warnf("Retry %d: update failed for %s", i+1, symbol)
		time.Sleep(time.Duration(i+1) * time.Second)
	}

	return sql.ErrConnDone
}

func safeInsertPriceHistory(ctx context.Context, db *sql.DB, symbol string, price float64) error {
	const maxRetries = 3

	for i := 0; i < maxRetries; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES ($1, $2, CURRENT_DATE)
			ON CONFLICT (stock_symbol, date) DO UPDATE 
			SET price = EXCLUDED.price
		`, symbol, price)

		if err == nil {
			return nil
		}

		logrus.WithError(err).Warnf("Retry %d: history insert failed for %s", i+1, symbol)
		time.Sleep(time.Duration(i+1) * time.Second)
	}

	return sql.ErrConnDone
}
