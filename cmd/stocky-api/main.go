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

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/LoganX64/stocky-api/internal/config"
	"github.com/LoganX64/stocky-api/internal/database"
	"github.com/LoganX64/stocky-api/internal/handlers/stocky"
	routes "github.com/LoganX64/stocky-api/internal/handlers/stocky"
	"github.com/LoganX64/stocky-api/internal/jobs"
	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/marketdata"
	"github.com/LoganX64/stocky-api/internal/metrics"
)

func main() {
	cfg := config.MustLoad()
	logMarketDataConfig(cfg)

	runAPI := cfg.Role == "api" || cfg.Role == "all"
	runJobs := cfg.Role == "worker" || cfg.Role == "all"

	logrus.WithFields(logrus.Fields{
		"role":           cfg.Role,
		"run_api":        runAPI,
		"run_jobs":       runJobs,
		"run_migrations": cfg.RunMigrations,
	}).Info("Application role configured")

	// =======================
	// Migrations
	// =======================
	shouldMigrate := cfg.RunMigrations || cfg.Role == "migrate"
	if shouldMigrate {
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
	} else {
		logrus.Info("Startup migrations disabled; assuming the schema is current")
	}
	if cfg.Role == "migrate" {
		logrus.Info("Migration-only role completed")
		return
	}

	// =======================
	// DB Pools
	// =======================

	// The API pool uses a short statement timeout so a slow report cannot
	// occupy a connection indefinitely. Background jobs get their own pool with
	// a longer timeout for rollover and reconciliation work. A worker-only
	// process does not open the API pool, and an API-only process does not open
	// the jobs pool.
	var apiDB *sql.DB
	if runAPI {
		var err error
		apiDB, err = database.Open(database.ConnectOptions{
			Host:                     cfg.Database.Host,
			Port:                     cfg.Database.DbPort,
			User:                     cfg.Database.User,
			Password:                 cfg.Database.Password,
			DBName:                   cfg.Database.DBName,
			SSLMode:                  cfg.Database.SSLMode,
			MaxOpenConns:             cfg.Database.MaxOpenConns,
			MaxIdleConns:             cfg.Database.MaxIdleConns,
			ConnMaxLifetime:          cfg.Database.ConnMaxLifetime,
			ConnMaxIdleTime:          cfg.Database.ConnMaxIdleTime,
			StatementTimeout:         cfg.Database.StatementTimeout,
			IdleInTransactionTimeout: cfg.Database.IdleInTransactionTimeout,
			ApplicationName:          "stocky-api",
		})
		if err != nil {
			logrus.Fatalf("database setup failed: %v", err)
		}
		defer func() {
			if err := apiDB.Close(); err != nil {
				logrus.WithError(err).Error("Failed to close API DB cleanly")
			}
		}()
		logrus.WithFields(logrus.Fields{
			"max_open":            cfg.Database.MaxOpenConns,
			"max_idle":            cfg.Database.MaxIdleConns,
			"statement_timeout":   cfg.Database.StatementTimeout.String(),
			"idle_in_transaction": cfg.Database.IdleInTransactionTimeout.String(),
		}).Info("API database pool ready")
	}

	var jobsDB *sql.DB
	if runJobs {
		var err error
		jobsDB, err = database.Open(database.ConnectOptions{
			Host:                     cfg.Database.Host,
			Port:                     cfg.Database.DbPort,
			User:                     cfg.Database.User,
			Password:                 cfg.Database.Password,
			DBName:                   cfg.Database.DBName,
			SSLMode:                  cfg.Database.SSLMode,
			MaxOpenConns:             cfg.Database.JobMaxOpenConns,
			MaxIdleConns:             cfg.Database.JobMaxIdleConns,
			ConnMaxLifetime:          cfg.Database.ConnMaxLifetime,
			ConnMaxIdleTime:          cfg.Database.ConnMaxIdleTime,
			StatementTimeout:         cfg.Database.JobStatementTimeout,
			IdleInTransactionTimeout: cfg.Database.IdleInTransactionTimeout,
			ApplicationName:          "stocky-jobs",
		})
		if err != nil {
			logrus.Fatalf("jobs database setup failed: %v", err)
		}
		defer func() {
			if err := jobsDB.Close(); err != nil {
				logrus.WithError(err).Error("Failed to close jobs DB cleanly")
			}
		}()
		logrus.WithFields(logrus.Fields{
			"max_open":          cfg.Database.JobMaxOpenConns,
			"max_idle":          cfg.Database.JobMaxIdleConns,
			"statement_timeout": cfg.Database.JobStatementTimeout.String(),
		}).Info("Jobs database pool ready")
	}

	readDB := apiDB
	if runAPI && cfg.Database.ReadDSN != "" {
		readPool, err := database.OpenDSN(cfg.Database.ReadDSN, database.PoolOptions{
			MaxOpenConns:             cfg.Database.ReadMaxOpenConns,
			MaxIdleConns:             cfg.Database.ReadMaxIdleConns,
			ConnMaxLifetime:          cfg.Database.ConnMaxLifetime,
			ConnMaxIdleTime:          cfg.Database.ConnMaxIdleTime,
			StatementTimeout:         cfg.Database.StatementTimeout,
			IdleInTransactionTimeout: cfg.Database.IdleInTransactionTimeout,
			ApplicationName:          "stocky-read",
		})
		if err != nil {
			logrus.Fatalf("read replica setup failed: %v", err)
		}
		defer func() {
			if err := readPool.Close(); err != nil {
				logrus.WithError(err).Error("Failed to close read replica DB cleanly")
			}
		}()
		readDB = readPool
		logrus.WithFields(logrus.Fields{
			"max_open": cfg.Database.ReadMaxOpenConns,
			"max_idle": cfg.Database.ReadMaxIdleConns,
		}).Warn("Read replica configured: reads may observe bounded replication lag")
	} else if runAPI {
		logrus.Info("Read replica disabled: reporting reads use the primary pool")
	}

	m := metrics.New()
	if apiDB != nil {
		m.RegisterDBPool("api", apiDB)
	}
	if jobsDB != nil {
		m.RegisterDBPool("jobs", jobsDB)
	}
	if runAPI && cfg.Database.ReadDSN != "" {
		m.RegisterDBPool("read", readDB)
	}

	// =======================
	// Root Context
	// =======================

	ctx, cancel := context.WithCancel(context.Background())

	// =======================
	// Background Price Jobs
	// =======================
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

	if runJobs && cfg.MarketData.Enabled {
		priceStore := jobs.NewPriceStore(jobsDB, nil)
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
		if !runJobs {
			logrus.Info("Market data jobs skipped: application role does not run background jobs")
		} else {
			logrus.Info("Market data jobs disabled by MARKET_DATA_ENABLED=false")
		}
		close(priceServiceDone)
		close(bhavcopyDone)
	}

	// =======================
	// Background Corporate Action Job
	// =======================
	corporateActionDone := make(chan struct{})
	if runJobs {
		corporateActionService := jobs.NewCorporateActionServiceWithOptions(jobsDB, &jobs.CorporateActionServiceOptions{
			Interval: cfg.Projection.CorporateActionInterval,
			Metrics:  m,
		})
		go func() {
			defer close(corporateActionDone)
			corporateActionService.Start(ctx)
		}()
	} else {
		close(corporateActionDone)
	}

	// =======================
	// Projection Queue Worker
	// =======================
	projectionRefreshDone := make(chan struct{})
	if runJobs {
		projectionRefreshService := jobs.NewProjectionRefreshServiceWithOptions(jobsDB, &jobs.ProjectionRefreshServiceOptions{
			Interval:           cfg.Projection.RefreshInterval,
			BatchSize:          cfg.Projection.RefreshBatch,
			MaxAttempts:        cfg.Projection.QueueMaxAttempts,
			ProcessedRetention: cfg.Projection.QueueRetention,
			FailedRetention:    cfg.Projection.QueueFailedRetention,
			AlertDepth:         cfg.Projection.QueueAlertDepth,
			AlertLag:           cfg.Projection.QueueAlertLag,
			SyncUserLimit:      cfg.Projection.SyncUserLimit,
			Metrics:            m,
		})
		go func() {
			defer close(projectionRefreshDone)
			projectionRefreshService.Start(ctx)
		}()
	} else {
		close(projectionRefreshDone)
	}

	// =======================
	// Projection Reconciliation Job
	// =======================
	reconciliationDone := make(chan struct{})
	if runJobs {
		reconciliationService := jobs.NewReconciliationServiceWithOptions(jobsDB, &jobs.ReconciliationServiceOptions{
			Interval:      cfg.Projection.ReconciliationInterval,
			SamplePercent: cfg.Projection.ReconciliationSamplePercent,
			MaxUsers:      cfg.Projection.ReconciliationMaxUsers,
			Metrics:       m,
		})
		go func() {
			defer close(reconciliationDone)
			reconciliationService.Start(ctx)
		}()
	} else {
		close(reconciliationDone)
	}

	// =======================
	// HTTP Server
	// =======================
	r := gin.Default()

	if cfg.HTTPServer.MetricsEnabled {
		r.Use(m.GinMiddleware())
	} else {
		logrus.Info("Prometheus /metrics endpoint disabled by METRICS_ENABLED=false")
	}
	r.Use(stocky.RequestTimeout(cfg.HTTPServer.RequestTimeout))
	if cfg.HTTPServer.MetricsEnabled {
		r.GET("/metrics", gin.WrapH(m.Handler()))
	}

	if runAPI {
		opts := []stocky.Option{
			stocky.WithReadDB(readDB),
			stocky.WithMetrics(m),
			stocky.WithCalendar(calendar),
			stocky.WithFreshness(freshness),
		}
		if cfg.HTTPServer.ETagEnabled {
			opts = append(opts, stocky.WithResponseCache(stocky.NewResponseCache(
				cfg.HTTPServer.ResponseCacheTTL,
				cfg.HTTPServer.ResponseCacheMax,
				m,
			)))
		}
		if cfg.HTTPServer.RateLimitEnabled {
			opts = append(opts, stocky.WithRateLimiter(stocky.NewUserRateLimiter(
				cfg.HTTPServer.RateLimitRPS,
				cfg.HTTPServer.RateLimitBurst,
				cfg.HTTPServer.RateLimitMaxKeys,
				m,
			)))
		}

		handler := stocky.NewHandler(apiDB, opts...)
		logrus.WithFields(logrus.Fields{
			"etag":              cfg.HTTPServer.ETagEnabled,
			"cache_ttl":         cfg.HTTPServer.ResponseCacheTTL.String(),
			"cache_max_entries": cfg.HTTPServer.ResponseCacheMax,
			"rate_limit":        cfg.HTTPServer.RateLimitEnabled,
			"rate_limit_rps":    cfg.HTTPServer.RateLimitRPS,
			"rate_limit_burst":  cfg.HTTPServer.RateLimitBurst,
		}).Info("Read-path HTTP features configured")
		routes.Routes(r, handler)
	} else {
		stocky.HealthRoute(r)
	}

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

	logrus.Info("Waiting for projection refresh worker to stop...")
	<-projectionRefreshDone
	logrus.Info("Projection refresh worker stopped")

	logrus.Info("Waiting for projection reconciliation service to stop...")
	<-reconciliationDone
	logrus.Info("Projection reconciliation service stopped")

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
