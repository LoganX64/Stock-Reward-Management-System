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
	// Background Price Job
	// =======================
	priceServiceDone := make(chan struct{})
	if cfg.MarketData.Enabled {
		provider, err := newQuoteProvider(cfg.MarketData)
		if err != nil {
			logrus.Fatalf("market data provider setup failed: %v", err)
		}

		calendar, err := marketcal.DefaultWithOverrides(cfg.MarketData.Holidays)
		if err != nil {
			logrus.Fatalf("market calendar setup failed: %v", err)
		}

		freshness := marketcal.DefaultFreshnessPolicy(calendar)
		freshness.MaxStale = cfg.MarketData.MaxStale
		freshness.CloseGrace = cfg.MarketData.CloseGrace

		cache := jobs.NewPriceCache()
		store := jobs.NewPriceStore(db, nil)
		priceService := jobs.NewPriceService(store, cache, provider, &jobs.PriceServiceOptions{
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
	} else {
		logrus.Info("Market data jobs disabled by MARKET_DATA_ENABLED=false")
		close(priceServiceDone)
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
	handler := stocky.NewHandler(db)
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
