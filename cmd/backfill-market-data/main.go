// Command backfill-market-data imports official NSE bhavcopy closes into
// stock_price_history for a date range.
//
// It writes history only: current prices are never changed. Official
// nse_bhavcopy rows are never overwritten by this command, and dates with no
// published file are reported as explicit gaps instead of being filled with
// synthetic prices.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/sirupsen/logrus"

	"github.com/LoganX64/stocky-api/internal/config"
	"github.com/LoganX64/stocky-api/internal/jobs"
	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
)

func main() {
	fromFlag := flag.String("from", "", "first market date to import (YYYY-MM-DD), inclusive")
	toFlag := flag.String("to", "", "last market date to import (YYYY-MM-DD), inclusive; defaults to yesterday")
	symbolsFlag := flag.String("symbols", "", "comma-separated symbols; defaults to all symbols in stock_prices")
	flag.Parse()

	if strings.TrimSpace(*fromFlag) == "" {
		logrus.Fatal("--from is required (YYYY-MM-DD)")
	}

	cfg := config.MustLoad()

	calendar, err := marketcal.DefaultWithOverrides(cfg.MarketData.Holidays)
	if err != nil {
		logrus.Fatalf("market calendar setup failed: %v", err)
	}

	from, err := parseMarketDate("--from", *fromFlag)
	if err != nil {
		logrus.Fatal(err)
	}
	to := calendar.Date(time.Now()).AddDate(0, 0, -1)
	if strings.TrimSpace(*toFlag) != "" {
		to, err = parseMarketDate("--to", *toFlag)
		if err != nil {
			logrus.Fatal(err)
		}
	}

	db, err := setupDB(cfg)
	if err != nil {
		logrus.Fatalf("database setup failed: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logrus.WithError(err).Error("Failed to close DB cleanly")
		}
	}()

	mapper, err := marketdata.NewSymbolMapper(marketdata.ExchangeNSE, "")
	if err != nil {
		logrus.Fatalf("bhavcopy symbol mapper setup failed: %v", err)
	}
	bhavcopy, err := marketdata.NewBhavcopyProvider(marketdata.BhavcopyOptions{
		BaseURL: cfg.MarketData.BhavcopyBaseURL,
		Mapper:  mapper,
		HTTP: marketdata.HTTPOptions{
			Timeout:     cfg.MarketData.HTTPTimeout,
			MaxAttempts: cfg.MarketData.MaxRetries,
			RetryBase:   cfg.MarketData.RetryBase,
			RateLimit:   cfg.MarketData.RateLimit,
			UserAgent:   cfg.MarketData.UserAgent,
		},
	})
	if err != nil {
		logrus.Fatalf("bhavcopy provider setup failed: %v", err)
	}

	provider, err := marketdata.NewBhavcopyHistoryProvider(bhavcopy, calendar)
	if err != nil {
		logrus.Fatalf("history provider setup failed: %v", err)
	}

	store := jobs.NewPriceStore(db, nil)
	service, err := jobs.NewHistoryBackfillService(store, provider, &jobs.HistoryBackfillOptions{
		Calendar: calendar,
	})
	if err != nil {
		logrus.Fatalf("history backfill setup failed: %v", err)
	}

	var symbols []string
	for _, raw := range strings.Split(*symbolsFlag, ",") {
		if symbol := strings.TrimSpace(raw); symbol != "" {
			symbols = append(symbols, symbol)
		}
	}

	result, err := service.Run(context.Background(), from, to, symbols)
	if err != nil {
		logrus.Fatalf("history backfill failed: %v", err)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		logrus.WithError(err).Warn("Could not encode the backfill summary as JSON")
	} else {
		fmt.Println(string(encoded))
	}

	if len(result.Failed) > 0 {
		logrus.Errorf("History backfill completed with %d failure(s); successful imports were kept", len(result.Failed))
		os.Exit(1)
	}
}

func parseMarketDate(flagName, raw string) (time.Time, error) {
	parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), marketcal.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: invalid date %q (want YYYY-MM-DD)", flagName, raw)
	}
	return parsed, nil
}

func setupDB(cfg *config.Config) (*sql.DB, error) {
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.Database.Host,
		cfg.Database.DbPort,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.DBName,
		cfg.Database.SSLMode,
	)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	return db, nil
}
