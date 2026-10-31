package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	_ "github.com/lib/pq"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/LoganX64/stocky-api/internal/config"
	"github.com/LoganX64/stocky-api/internal/handlers/stocky"
	routes "github.com/LoganX64/stocky-api/internal/handlers/stocky"
	"github.com/LoganX64/stocky-api/internal/jobs"
	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
)

func main() {
	cfg := config.MustLoad()
	logMarketDataConfig(cfg)

	// =======================
	// DB Setup
	// =======================
	db, err := setupDB(cfg)
	if err != nil {
		logrus.Fatalf("database setup failed: %v", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			logrus.WithError(err).Error("Failed to close DB cleanly")
		}
	}()

	// =======================
	// Migrations
	// =======================
	dbURL := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Host,
		cfg.Database.DbPort,
		cfg.Database.DBName,
		cfg.Database.SSLMode,
	)
	if err := runMigrations(dbURL, cfg.MigrationPath); err != nil {
		logrus.Fatalf("migrations failed: %v", err)
	}
	logrus.Info("Migrations applied successfully")

	// =======================
	// Root Context
	// =======================

	ctx, cancel := context.WithCancel(context.Background())

	// =======================
	// Background Price Jobs
	// =======================
	priceStore := jobs.NewPriceStore(db, nil)
	priceServiceDone := make(chan struct{})
	bhavcopyDone := make(chan struct{})

	// The market calendar and freshness policy are shared by the scheduler, the
	// EOD ingestion job, and the read API so staleness cannot drift between
	// them. They are built even when background jobs are disabled because the
	// API still classifies stored quotes.
	calendar, err := marketcal.DefaultWithOverrides(cfg.MarketData.Holidays)
	if err != nil {
		logrus.Fatalf("market calendar setup failed: %v", err)
	}

	freshness := marketcal.DefaultFreshnessPolicy(calendar)
	freshness.MaxStale = cfg.MarketData.MaxStale
	freshness.CloseGrace = cfg.MarketData.CloseGrace

	if cfg.MarketData.Enabled {
		provider, err := newQuoteProvider(cfg.MarketData)
		if err != nil {
			logrus.Fatalf("market data provider setup failed: %v", err)
		}

		cache := jobs.NewPriceCache()
		priceService := jobs.NewPriceService(priceStore, cache, provider, &jobs.PriceServiceOptions{
			TickInterval:    cfg.MarketData.TickInterval,
			MarketHoursOnly: cfg.MarketData.MarketHoursOnly,
			FinalQuoteDelay: cfg.MarketData.FinalQuoteDelay,
			Calendar:        calendar,
			Freshness:       freshness,
		})

		logrus.WithField("provider", provider.Name()).Info("Market data provider configured")
		go func() {
			defer close(priceServiceDone)
			priceService.Start(ctx)
		}()

		startBhavcopyService(ctx, cfg, priceStore, calendar, bhavcopyDone)
	} else {
		logrus.Info("Market data jobs disabled by MARKET_DATA_ENABLED=false")
		close(priceServiceDone)
		close(bhavcopyDone)
	}

	// =======================
	// Background Corporate Action Job
	// =======================
	corporateActionService := jobs.NewCorporateActionService(db)
	corporateActionDone := make(chan struct{})
	go func() {
		defer close(corporateActionDone)
		corporateActionService.Start(ctx)
	}()

	// =======================
	// HTTP Server
	// =======================
	r := gin.Default()
	handler := stocky.NewHandler(db,
		stocky.WithCalendar(calendar),
		stocky.WithFreshness(freshness),
	)
	// Register routes with the handler
	routes.Routes(r, handler)

	port := cfg.HTTPServer.Port
	if port == "" {
		port = ":8080"
	}

	srv := &http.Server{
		Addr:         port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	httpErr := make(chan error, 1)
	go func() {
		logrus.Infof("Starting HTTP server on %s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
		close(httpErr)
	}()

	// =======================
	// Graceful Shutdown
	// =======================
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logrus.Infof("Received signal %s — shutting down", sig)
	case err := <-httpErr:
		logrus.WithError(err).Error("HTTP server exited unexpectedly")
	}

	cancel()

	logrus.Info("Waiting for price service to stop...")
	<-priceServiceDone
	logrus.Info("Price service stopped")

	logrus.Info("Waiting for bhavcopy service to stop...")
	<-bhavcopyDone
	logrus.Info("Bhavcopy service stopped")

	logrus.Info("Waiting for corporate action processor to stop...")
	<-corporateActionDone
	logrus.Info("Corporate action processor stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logrus.WithError(err).Error("HTTP server forced to shutdown")
	} else {
		logrus.Info("HTTP server stopped cleanly")
	}

	logrus.Info("Server exited properly")

}

// =======================
// DB Setup
// =======================

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

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	logrus.Info("Connected to database successfully")
	return db, nil
}

// =======================
// Migrations
// =======================

// =======================
// Market Data Provider
// =======================

func newQuoteProvider(md config.MarketData) (marketdata.QuoteProvider, error) {
	switch md.Provider {
	case "simulator":
		provider, err := marketdata.NewSimulatorWithOptions(marketdata.SimulatorOptions{
			Exchange:     md.Exchange,
			SymbolSuffix: md.SymbolSuffix,
		})
		if err != nil {
			return nil, err
		}
		return provider, nil
	case "yahoo":
		mapper, err := marketdata.NewSymbolMapper(md.Exchange, md.SymbolSuffix)
		if err != nil {
			return nil, err
		}
		provider, err := marketdata.NewYahooProvider(marketdata.YahooOptions{
			BaseURL: md.YahooBaseURL,
			Mapper:  mapper,
			HTTP: marketdata.HTTPOptions{
				Timeout:     md.HTTPTimeout,
				MaxAttempts: md.MaxRetries,
				RetryBase:   md.RetryBase,
				RateLimit:   md.RateLimit,
				UserAgent:   md.UserAgent,
			},
		})
		if err != nil {
			return nil, err
		}
		return provider, nil
	default:
		return nil, fmt.Errorf("unsupported market data provider %q", md.Provider)
	}
}

func newBhavcopyProvider(md config.MarketData) (marketdata.DailyCloseProvider, error) {
	mapper, err := marketdata.NewSymbolMapper(md.Exchange, md.SymbolSuffix)
	if err != nil {
		return nil, err
	}
	provider, err := marketdata.NewBhavcopyProvider(marketdata.BhavcopyOptions{
		BaseURL: md.BhavcopyBaseURL,
		Mapper:  mapper,
		HTTP: marketdata.HTTPOptions{
			Timeout:     md.HTTPTimeout,
			MaxAttempts: md.MaxRetries,
			RetryBase:   md.RetryBase,
			RateLimit:   md.RateLimit,
			UserAgent:   md.UserAgent,
		},
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// startBhavcopyService launches the official NSE EOD ingestion job when the
// market data provider is Yahoo. Simulator mode stays fully offline, so
// bhavcopy is skipped even when MARKET_DATA_BHAVCOPY_ENABLED=true.
func startBhavcopyService(
	ctx context.Context,
	cfg *config.Config,
	store jobs.PriceStore,
	calendar *marketcal.Calendar,
	done chan struct{},
) {
	if !cfg.MarketData.BhavcopyEnabled {
		logrus.Info("Bhavcopy EOD ingestion disabled by MARKET_DATA_BHAVCOPY_ENABLED=false")
		close(done)
		return
	}
	if cfg.MarketData.Provider != "yahoo" {
		logrus.WithField("provider", cfg.MarketData.Provider).
			Warn("Bhavcopy EOD ingestion skipped because MARKET_DATA_PROVIDER is not yahoo; simulator mode stays offline")
		close(done)
		return
	}

	provider, err := newBhavcopyProvider(cfg.MarketData)
	if err != nil {
		logrus.Fatalf("bhavcopy provider setup failed: %v", err)
	}

	service, err := jobs.NewBhavcopyService(store, provider, &jobs.BhavcopyServiceOptions{
		Calendar:      calendar,
		FirstAttempt:  cfg.MarketData.BhavcopyTime,
		RetryUntil:    cfg.MarketData.BhavcopyRetryUntil,
		RetryInterval: cfg.MarketData.BhavcopyRetryInterval,
	})
	if err != nil {
		logrus.Fatalf("bhavcopy service setup failed: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"provider":       provider.Name(),
		"first_attempt":  cfg.MarketData.BhavcopyTime,
		"retry_until":    cfg.MarketData.BhavcopyRetryUntil,
		"retry_interval": cfg.MarketData.BhavcopyRetryInterval.String(),
	}).Info("Bhavcopy EOD service configured")

	go func() {
		defer close(done)
		service.Start(ctx)
	}()
}

// logMarketDataConfig records the effective market-data configuration at
// startup. It deliberately omits secrets and makes simulator mode and disabled
// bhavcopy explicit so an operator can explain where prices come from.
func logMarketDataConfig(cfg *config.Config) {
	md := cfg.MarketData
	logrus.WithFields(logrus.Fields{
		"env":                     cfg.Env,
		"market_data_enabled":     md.Enabled,
		"provider":                md.Provider,
		"exchange":                md.Exchange,
		"symbol_suffix":           md.SymbolSuffix,
		"tick_interval":           md.TickInterval.String(),
		"market_hours_only":       md.MarketHoursOnly,
		"final_quote_delay":       md.FinalQuoteDelay.String(),
		"max_stale":               md.MaxStale.String(),
		"close_grace":             md.CloseGrace.String(),
		"http_timeout":            md.HTTPTimeout.String(),
		"max_retries":             md.MaxRetries,
		"retry_base":              md.RetryBase.String(),
		"rate_limit":              md.RateLimit.String(),
		"yahoo_base_url":          md.YahooBaseURL,
		"bhavcopy_enabled":        md.BhavcopyEnabled,
		"bhavcopy_base_url":       md.BhavcopyBaseURL,
		"bhavcopy_time":           md.BhavcopyTime,
		"bhavcopy_retry_until":    md.BhavcopyRetryUntil,
		"bhavcopy_retry_interval": md.BhavcopyRetryInterval.String(),
		"holiday_override_count":  len(md.Holidays),
	}).Info("Market-data configuration loaded")

	switch md.Provider {
	case "simulator":
		logrus.Warn("Simulator mode is active: prices are synthetic random-walk values, not Indian market data")
		if md.BhavcopyEnabled {
			logrus.Warn("Bhavcopy EOD ingestion will be skipped in simulator mode; set MARKET_DATA_BHAVCOPY_ENABLED=false to silence this warning")
		}
	case "yahoo":
		if md.BhavcopyEnabled {
			logrus.Info("Yahoo Finance provides unofficial, delayed current quotes; NSE bhavcopy supplies the official end-of-day close")
		} else {
			logrus.Info("Yahoo Finance provides unofficial, delayed current quotes; official NSE bhavcopy ingestion is disabled")
		}
	}

	if !md.Enabled {
		logrus.Info("Market-data background jobs are disabled by MARKET_DATA_ENABLED=false; stored prices will still be served with provenance and freshness")
	}
}

func runMigrations(dbURL, migrationPath string) error {
	m, err := migrate.New("file://"+migrationPath, dbURL)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}

	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			logrus.WithError(srcErr).Warn("Migration source close error")
		}
		if dbErr != nil {
			logrus.WithError(dbErr).Warn("Migration DB close error")
		}
	}()

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			logrus.Info("No new migrations to apply")
			return nil
		}
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}
