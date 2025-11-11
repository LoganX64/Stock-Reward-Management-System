package jobs

import (
	"database/sql"
	"math/rand"
	"sync"
	"time"

	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/robfig/cron/v3"
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

var (
	priceCache           = &PriceCache{prices: make(map[string]CachedPrice)}
	maxPriceStaleMinutes = 120
)

func StartPriceUpdater(db *sql.DB) {

	initializePriceCache(db)

	c := cron.New(cron.WithChain(
		cron.Recover(cron.DefaultLogger),
	))

	_, err := c.AddFunc("@every 10s", // "@every 10s" makes it 10 seconds and "0 * * * *" is every hour
		func() {
			updatePrices(db)
		})
	if err != nil {
		logrus.WithError(err).Fatal("Failed to schedule price updater")
	}
	c.Start()
	logrus.Info("Hourly price updater started")
}

func initializePriceCache(db *sql.DB) {
	rows, err := db.Query(`
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
	if cached, ok := pc.prices[symbol]; ok {
		return cached.Price, cached.UpdatedAt, true
	}
	return 0, time.Time{}, false
}

func updatePrices(db *sql.DB) {
	logrus.Info("Updating stock prices...")

	rows, err := db.Query(`SELECT stock_symbol, price FROM stock_prices`)
	if err != nil {
		logrus.WithError(err).Error("Failed to fetch stock prices")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var symbol string
		var oldPrice float64
		if err := rows.Scan(&symbol, &oldPrice); err != nil {
			logrus.WithError(err).Warn("Failed to scan stock price")
			continue
		}

		newPrice, err := getLatestPrice(symbol, oldPrice)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to get new price for %s, using fallback", symbol)
			factor := 0.99 + rand.Float64()*0.02
			newPrice = utils.RoundAmount(oldPrice * factor)
		}

		updateSuccess := false
		if err := safeUpdatePrice(db, symbol, newPrice); err != nil {

			if cachedPrice, cachedTime, ok := priceCache.GetPrice(symbol); ok {
				staleness := time.Since(cachedTime)
				if staleness.Minutes() < float64(maxPriceStaleMinutes) {
					logrus.Infof("Using cached price for %s (%.2f mins old)", symbol, staleness.Minutes())
					if err := safeUpdatePrice(db, symbol, cachedPrice); err != nil {
						logrus.Warnf("Failed to update with cached price for %s", symbol)
					} else {
						newPrice = cachedPrice
						updateSuccess = true
					}
				} else {
					logrus.Warnf("Cached price for %s too old (%.2f mins), keeping last DB price", symbol, staleness.Minutes())
				}
			}
			if !updateSuccess {
				continue
			}
		} else {
			updateSuccess = true
		}

		if updateSuccess {
			priceCache.SetPrice(symbol, newPrice, time.Now())
		}

		if err := safeInsertPriceHistory(db, symbol, newPrice); err != nil {
			logrus.WithError(err).Errorf("Failed to insert/update price history for %s", symbol)
			continue
		}

		logrus.Infof("Updated %s: %.2f -> %.2f", symbol, oldPrice, newPrice)
	}
}

func getLatestPrice(symbol string, lastPrice float64) (float64, error) {

	if rand.Float64() < 0.1 {
		return 0, sql.ErrConnDone
	}

	factor := 0.95 + rand.Float64()*0.10
	return utils.RoundAmount(lastPrice * factor), nil
}

func safeUpdatePrice(db *sql.DB, symbol string, newPrice float64) error {
	const maxRetries = 3
	for i := 0; i < maxRetries; i++ {
		_, err := db.Exec(`UPDATE stock_prices SET price=$1, updated_at=NOW() WHERE stock_symbol=$2`, newPrice, symbol)
		if err == nil {
			return nil
		}
		logrus.WithError(err).Warnf("Retry %d: Failed to update price for %s", i+1, symbol)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return sql.ErrConnDone
}

func safeInsertPriceHistory(db *sql.DB, symbol string, price float64) error {
	const maxRetries = 3
	for i := 0; i < maxRetries; i++ {
		_, err := db.Exec(`
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES ($1, $2, CURRENT_DATE)
			ON CONFLICT (stock_symbol, date) DO UPDATE 
			SET price = EXCLUDED.price
		`, symbol, price)
		if err == nil {
			return nil
		}
		logrus.WithError(err).Warnf("Retry %d: Failed to insert price history for %s", i+1, symbol)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return sql.ErrConnDone
}
