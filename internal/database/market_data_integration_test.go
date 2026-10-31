// PostgreSQL integration tests for A3 Phase 2: market-data provenance and the
// transactional price store.
//
// These tests are opt-in like the other database integration tests:
//
//	TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' \
//	  go test ./internal/database -run MarketData -v
//
// Every top-level test creates and drops its own disposable database. The
// tests skip when TEST_DATABASE_URL is unset.
package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/jobs"
	"github.com/LoganX64/stocky-api/internal/marketdata"
)

var marketDataIntegrationNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func marketDataQuote(symbol, price string, asOf time.Time, source string) marketdata.Quote {
	providerSymbol := symbol + ".NS"
	if source == marketdata.SourceNSEBhavcopy {
		providerSymbol = symbol
	}
	return marketdata.Quote{
		Symbol:         symbol,
		ProviderSymbol: providerSymbol,
		Price:          decimal.RequireFromString(price),
		Currency:       marketdata.CurrencyINR,
		Exchange:       marketdata.ExchangeNSE,
		AsOf:           asOf,
		FetchedAt:      asOf.Add(30 * time.Second),
		Source:         source,
	}
}

func newMarketDataIntegrationStore(db *sql.DB) *jobs.SQLPriceStore {
	return jobs.NewPriceStore(db, &jobs.PriceStoreOptions{
		Now: func() time.Time { return marketDataIntegrationNow },
	})
}

func provenanceColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()

	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = $1
		  AND column_name = $2
	`, table, column).Scan(&count); err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	return count > 0
}

func TestMarketDataProvenanceMigrationIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, 24)

	db := openIntegrationDB(t, dbURL)
	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, updated_at)
		VALUES
			('MDPROV', 100.5000, TIMESTAMPTZ '2026-10-28 09:00:00+00'),
			('MDPROVNULL', 200.0000, NULL)
	`)
	mustExec(t, db, `
		INSERT INTO stock_price_history (stock_symbol, price, date, created_at)
		VALUES
			('MDPROV', 100.5000, DATE '2026-10-28', TIMESTAMPTZ '2026-10-28 09:00:00+00'),
			('MDPROVNULL', 200.0000, DATE '2026-10-28', NULL)
	`)
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration database connection: %v", err)
	}

	applyMigrations(t, dbURL, 25)
	db = openIntegrationDB(t, dbURL)

	for _, table := range []string{"stock_prices", "stock_price_history"} {
		for column, wantType := range map[string]string{
			"source":          "text",
			"fetched_at":      "timestamp with time zone",
			"provider_symbol": "text",
			"exchange":        "text",
			"currency":        "text",
		} {
			if !provenanceColumnExists(t, db, table, column) {
				t.Fatalf("%s.%s is missing after migration 0025", table, column)
			}
			if got := columnType(t, db, table, column); got != wantType {
				t.Errorf("%s.%s type = %q, want %q", table, column, got, wantType)
			}
		}
	}

	for _, constraint := range []string{
		"stock_prices_price_positive",
		"stock_prices_source_not_blank",
		"stock_prices_currency_inr",
		"stock_prices_exchange_valid",
		"stock_price_history_price_positive",
		"stock_price_history_source_not_blank",
		"stock_price_history_currency_inr",
		"stock_price_history_exchange_valid",
	} {
		if !constraintExists(t, db, constraint) {
			t.Errorf("constraint %s is missing after migration 0025", constraint)
		}
	}

	if !functionExists(t, db, "market_data_source_rank") {
		t.Fatal("market_data_source_rank is missing after migration 0025")
	}
	for source, wantRank := range map[string]int{
		"legacy":       0,
		"seed":         0,
		"simulator":    1,
		"yahoo":        2,
		"nse_bhavcopy": 3,
		"unknown":      0,
	} {
		var rank int
		if err := db.QueryRow(`SELECT market_data_source_rank($1)`, source).Scan(&rank); err != nil {
			t.Fatalf("rank source %s: %v", source, err)
		}
		if rank != wantRank {
			t.Errorf("market_data_source_rank(%q) = %d, want %d", source, rank, wantRank)
		}
	}

	var (
		source     string
		currency   string
		fetchedAt  time.Time
		provider   sql.NullString
		exchange   sql.NullString
		historySrc string
		historyAt  time.Time
	)
	if err := db.QueryRow(`
		SELECT source, currency, fetched_at, provider_symbol, exchange
		FROM stock_prices
		WHERE stock_symbol = 'MDPROV'
	`).Scan(&source, &currency, &fetchedAt, &provider, &exchange); err != nil {
		t.Fatalf("read migrated current price: %v", err)
	}
	if source != "legacy" || currency != "INR" {
		t.Errorf("migrated current provenance = source %q currency %q, want legacy/INR", source, currency)
	}
	if provider.Valid || exchange.Valid {
		t.Errorf("legacy provider_symbol/exchange = %v/%v, want NULL/NULL", provider, exchange)
	}
	if want := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC); !fetchedAt.Equal(want) {
		t.Errorf("migrated current fetched_at = %s, want %s", fetchedAt, want)
	}
	if got := scalarFloat(t, db, `SELECT price::float8 FROM stock_prices WHERE stock_symbol = 'MDPROV'`); got != 100.5 {
		t.Errorf("migrated current price = %v, want 100.5", got)
	}

	if err := db.QueryRow(`
		SELECT source, fetched_at
		FROM stock_price_history
		WHERE stock_symbol = 'MDPROV' AND date = DATE '2026-10-28'
	`).Scan(&historySrc, &historyAt); err != nil {
		t.Fatalf("read migrated history price: %v", err)
	}
	if historySrc != "legacy" {
		t.Errorf("migrated history source = %q, want legacy", historySrc)
	}
	if want := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC); !historyAt.Equal(want) {
		t.Errorf("migrated history fetched_at = %s, want %s", historyAt, want)
	}
	if got := scalarFloat(t, db, `SELECT price::float8 FROM stock_price_history WHERE stock_symbol = 'MDPROV' AND date = DATE '2026-10-28'`); got != 100.5 {
		t.Errorf("migrated history price = %v, want 100.5", got)
	}

	// A legacy row without a timestamp still gets a non-NULL fetched_at.
	if !scalarBool(t, db, `SELECT fetched_at IS NOT NULL FROM stock_prices WHERE stock_symbol = 'MDPROVNULL'`) {
		t.Error("fetched_at is NULL for a legacy current row without updated_at")
	}
	if !scalarBool(t, db, `SELECT fetched_at IS NOT NULL FROM stock_price_history WHERE stock_symbol = 'MDPROVNULL'`) {
		t.Error("fetched_at is NULL for a legacy history row without created_at")
	}

	for name, query := range map[string]string{
		"non-positive current price": `INSERT INTO stock_prices (stock_symbol, price) VALUES ('MDZERO', 0)`,
		"blank current source":       `INSERT INTO stock_prices (stock_symbol, price, source) VALUES ('MDBLANK', 10, ' ')`,
		"non-INR current currency":   `INSERT INTO stock_prices (stock_symbol, price, currency) VALUES ('MDUSD', 10, 'USD')`,
		"unknown current exchange":   `INSERT INTO stock_prices (stock_symbol, price, exchange) VALUES ('MDNYSE', 10, 'NYSE')`,
		"non-positive history price": `INSERT INTO stock_price_history (stock_symbol, price, date) VALUES ('MDZEROH', 0, DATE '2026-10-28')`,
		"blank history source":       `INSERT INTO stock_price_history (stock_symbol, price, date, source) VALUES ('MDBLANKH', 10, DATE '2026-10-28', ' ')`,
		"non-INR history currency":   `INSERT INTO stock_price_history (stock_symbol, price, date, currency) VALUES ('MDUSDH', 10, DATE '2026-10-28', 'USD')`,
		"unknown history exchange":   `INSERT INTO stock_price_history (stock_symbol, price, date, exchange) VALUES ('MDNYSEH', 10, DATE '2026-10-28', 'NYSE')`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Errorf("%s: expected constraint violation, got nil", name)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close post-migration database connection: %v", err)
	}
	applyMigrations(t, dbURL, 24)
	db = openIntegrationDB(t, dbURL)

	for _, table := range []string{"stock_prices", "stock_price_history"} {
		for _, column := range []string{"source", "fetched_at", "provider_symbol", "exchange", "currency"} {
			if provenanceColumnExists(t, db, table, column) {
				t.Errorf("%s.%s still exists after the down migration", table, column)
			}
		}
	}
	if functionExists(t, db, "market_data_source_rank") {
		t.Error("market_data_source_rank still exists after the down migration")
	}
	for _, table := range []string{
		"today_rewards",
		"user_portfolio",
		"historical_rewards",
		"stock_prices_adjusted",
		"stock_price_history_adjusted",
	} {
		if provenanceColumnExists(t, db, table, "source") || provenanceColumnExists(t, db, table, "price_source") {
			t.Errorf("%s still exposes provenance after the down migration", table)
		}
	}
	if got := scalarFloat(t, db, `SELECT price::float8 FROM stock_prices WHERE stock_symbol = 'MDPROV'`); got != 100.5 {
		t.Errorf("current price after down migration = %v, want 100.5", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM historical_rewards_as_of(NULL::DATE)`); got != 0 {
		t.Errorf("historical_rewards_as_of rows after down migration = %d, want 0", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM today_rewards`); got != 0 {
		t.Errorf("today_rewards rows after down migration = %d, want 0", got)
	}
}

func TestMarketDataPriceStorePrecedenceIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	store := newMarketDataIntegrationStore(db)
	ctx := context.Background()

	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, updated_at)
		VALUES ('MDSEQ', 100, TIMESTAMPTZ '2026-10-29 10:00:00+00')
	`)
	mustExec(t, db, `
		INSERT INTO stock_price_history (stock_symbol, price, date, created_at)
		VALUES ('MDSEQ', 100, DATE '2026-10-29', TIMESTAMPTZ '2026-10-29 10:00:00+00')
	`)

	// A newer Yahoo quote replaces the legacy current row and creates history
	// for the quote's own IST market date.
	yahoo := marketDataQuote(
		"MDSEQ",
		"150",
		time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), // 15:30 IST
		marketdata.SourceYahoo,
	)
	result, err := store.SaveQuote(ctx, yahoo)
	if err != nil {
		t.Fatalf("save first Yahoo quote: %v", err)
	}
	if !result.CurrentAccepted || !result.HistoryChanged {
		t.Fatalf("first Yahoo result = %+v, want both accepted", result)
	}
	assertCurrentPrice(t, db, "MDSEQ", "yahoo", 150, yahoo.AsOf)
	assertHistoryPrice(t, db, "MDSEQ", "2026-10-30", "yahoo", 150, yahoo.AsOf)

	// An older quote must not move timestamps backwards.
	older := marketDataQuote(
		"MDSEQ",
		"140",
		time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
		marketdata.SourceYahoo,
	)
	result, err = store.SaveQuote(ctx, older)
	if err != nil {
		t.Fatalf("save older Yahoo quote: %v", err)
	}
	if result.CurrentAccepted || result.HistoryChanged {
		t.Errorf("older Yahoo result = %+v, want both rejected", result)
	}
	assertCurrentPrice(t, db, "MDSEQ", "yahoo", 150, yahoo.AsOf)
	assertHistoryPrice(t, db, "MDSEQ", "2026-10-30", "yahoo", 150, yahoo.AsOf)

	// Bhavcopy wins its trading date even when the timestamp is not newer.
	bhavcopy := marketDataQuote(
		"MDSEQ",
		"149",
		time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
		marketdata.SourceNSEBhavcopy,
	)
	result, err = store.SaveQuote(ctx, bhavcopy)
	if err != nil {
		t.Fatalf("save bhavcopy quote: %v", err)
	}
	if !result.CurrentAccepted || !result.HistoryChanged {
		t.Errorf("bhavcopy result = %+v, want both accepted", result)
	}
	assertCurrentPrice(t, db, "MDSEQ", "nse_bhavcopy", 149, bhavcopy.AsOf)
	assertHistoryPrice(t, db, "MDSEQ", "2026-10-30", "nse_bhavcopy", 149, bhavcopy.AsOf)

	// Yahoo cannot overwrite a bhavcopy row for the same date, even with a
	// later timestamp.
	sameDayYahoo := marketDataQuote(
		"MDSEQ",
		"151",
		time.Date(2026, 10, 30, 10, 5, 0, 0, time.UTC),
		marketdata.SourceYahoo,
	)
	result, err = store.SaveQuote(ctx, sameDayYahoo)
	if err != nil {
		t.Fatalf("save same-day Yahoo quote: %v", err)
	}
	if result.CurrentAccepted || result.HistoryChanged {
		t.Errorf("same-day Yahoo result = %+v, want both rejected", result)
	}
	assertCurrentPrice(t, db, "MDSEQ", "nse_bhavcopy", 149, bhavcopy.AsOf)
	assertHistoryPrice(t, db, "MDSEQ", "2026-10-30", "nse_bhavcopy", 149, bhavcopy.AsOf)

	// A quote from the next session may take over the current price while the
	// old bhavcopy history row remains intact.
	nextSession := marketDataQuote(
		"MDSEQ",
		"152",
		time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		marketdata.SourceYahoo,
	)
	result, err = store.SaveQuote(ctx, nextSession)
	if err != nil {
		t.Fatalf("save next-session Yahoo quote: %v", err)
	}
	if !result.CurrentAccepted || !result.HistoryChanged {
		t.Errorf("next-session Yahoo result = %+v, want both accepted", result)
	}
	assertCurrentPrice(t, db, "MDSEQ", "yahoo", 152, nextSession.AsOf)
	assertHistoryPrice(t, db, "MDSEQ", "2026-10-30", "nse_bhavcopy", 149, bhavcopy.AsOf)
	assertHistoryPrice(t, db, "MDSEQ", "2026-11-02", "yahoo", 152, nextSession.AsOf)

	// Replaying the same quote is a no-op for both rows.
	result, err = store.SaveQuote(ctx, nextSession)
	if err != nil {
		t.Fatalf("replay next-session Yahoo quote: %v", err)
	}
	if result.CurrentAccepted || result.HistoryChanged {
		t.Errorf("duplicate quote result = %+v, want both rejected", result)
	}

	// A late quote for the bhavcopy date still cannot replace it.
	lateYahoo := marketDataQuote(
		"MDSEQ",
		"153",
		time.Date(2026, 10, 30, 10, 6, 0, 0, time.UTC),
		marketdata.SourceYahoo,
	)
	result, err = store.SaveQuote(ctx, lateYahoo)
	if err != nil {
		t.Fatalf("save late Yahoo quote: %v", err)
	}
	if result.CurrentAccepted || result.HistoryChanged {
		t.Errorf("late Yahoo result = %+v, want both rejected", result)
	}
	assertHistoryPrice(t, db, "MDSEQ", "2026-10-30", "nse_bhavcopy", 149, bhavcopy.AsOf)
}

func TestMarketDataPriceStoreHistoryDateUsesISTIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	store := newMarketDataIntegrationStore(db)

	// 20:00 UTC on 30 October is 01:30 IST on 31 October. The history row must
	// belong to 31 October, not 30 October.
	quote := marketDataQuote(
		"MDIST",
		"250",
		time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC),
		marketdata.SourceYahoo,
	)
	result, err := store.SaveQuote(context.Background(), quote)
	if err != nil {
		t.Fatalf("save IST-boundary quote: %v", err)
	}
	if !result.CurrentAccepted || !result.HistoryChanged {
		t.Fatalf("IST-boundary result = %+v, want both accepted", result)
	}
	assertHistoryPrice(t, db, "MDIST", "2026-10-31", "yahoo", 250, quote.AsOf)
	if got := countRows(t, db, `SELECT COUNT(*) FROM stock_price_history WHERE stock_symbol = 'MDIST'`); got != 1 {
		t.Errorf("history row count = %d, want 1", got)
	}
}

func TestMarketDataPriceStoreAtomicityIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	store := newMarketDataIntegrationStore(db)

	mustExec(t, db, `
		CREATE OR REPLACE FUNCTION fail_market_data_history()
		RETURNS TRIGGER
		LANGUAGE plpgsql
		AS $$
		BEGIN
			RAISE EXCEPTION 'forced history failure';
		END;
		$$;
	`)
	mustExec(t, db, `
		CREATE TRIGGER trg_fail_market_data_history
		BEFORE INSERT OR UPDATE ON stock_price_history
		FOR EACH ROW EXECUTE FUNCTION fail_market_data_history();
	`)

	quote := marketDataQuote(
		"MDATOMIC",
		"300",
		time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
		marketdata.SourceYahoo,
	)
	if _, err := store.SaveQuote(context.Background(), quote); err == nil {
		t.Fatal("SaveQuote returned nil error, want forced history failure")
	}

	mustExec(t, db, `DROP TRIGGER trg_fail_market_data_history ON stock_price_history`)
	mustExec(t, db, `DROP FUNCTION fail_market_data_history()`)

	if got := countRows(t, db, `SELECT COUNT(*) FROM stock_prices WHERE stock_symbol = 'MDATOMIC'`); got != 0 {
		t.Errorf("current row count after rollback = %d, want 0", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM stock_price_history WHERE stock_symbol = 'MDATOMIC'`); got != 0 {
		t.Errorf("history row count after rollback = %d, want 0", got)
	}
}

func TestMarketDataProvenanceViewsIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	userID := insertIntegrationUser(t, db, "market-data-views")

	insertTodayReward(t, db, userID, "MDVIEW", 2)
	insertTodayReward(t, db, userID, "MDMISSING", 3)

	updatedAt := time.Date(2026, 10, 31, 5, 0, 0, 0, time.UTC)
	fetchedAt := updatedAt.Add(45 * time.Second)
	mustExec(t, db, `
		INSERT INTO stock_prices
			(stock_symbol, price, updated_at, source, fetched_at,
			 provider_symbol, exchange, currency)
		VALUES
			('MDVIEW', 300, $1, 'yahoo', $2, 'MDVIEW.NS', 'NSE', 'INR')
	`, updatedAt, fetchedAt)

	var (
		price      float64
		status     string
		priceAsOf  time.Time
		priceSrc   sql.NullString
		priceFetch sql.NullTime
	)
	if err := db.QueryRow(`
		SELECT current_price::float8, price_status, price_as_of, price_source, price_fetched_at
		FROM today_rewards
		WHERE user_id = $1 AND stock_symbol = 'MDVIEW'
	`, userID).Scan(&price, &status, &priceAsOf, &priceSrc, &priceFetch); err != nil {
		t.Fatalf("read today_rewards provenance: %v", err)
	}
	if price != 300 || status != "available" {
		t.Errorf("today_rewards price/status = %v/%q, want 300/available", price, status)
	}
	if !priceAsOf.Equal(updatedAt) {
		t.Errorf("today_rewards price_as_of = %s, want %s", priceAsOf, updatedAt)
	}
	if !priceSrc.Valid || priceSrc.String != "yahoo" {
		t.Errorf("today_rewards price_source = %v, want yahoo", priceSrc)
	}
	if !priceFetch.Valid || !priceFetch.Time.Equal(fetchedAt) {
		t.Errorf("today_rewards price_fetched_at = %v, want %s", priceFetch, fetchedAt)
	}

	var missingStatus string
	var missingSource, missingFetched sql.NullString
	if err := db.QueryRow(`
		SELECT price_status, price_source, price_fetched_at::text
		FROM today_rewards
		WHERE user_id = $1 AND stock_symbol = 'MDMISSING'
	`, userID).Scan(&missingStatus, &missingSource, &missingFetched); err != nil {
		t.Fatalf("read missing today_rewards provenance: %v", err)
	}
	if missingStatus != "missing" || missingSource.Valid || missingFetched.Valid {
		t.Errorf("missing today_rewards = status %q source %v fetched %v, want missing/NULL/NULL",
			missingStatus, missingSource, missingFetched)
	}

	var (
		portfolioSource sql.NullString
		portfolioFetch  sql.NullTime
	)
	if err := db.QueryRow(`
		SELECT price_source, price_fetched_at
		FROM user_portfolio
		WHERE user_id = $1 AND stock_symbol = 'MDVIEW'
	`, userID).Scan(&portfolioSource, &portfolioFetch); err != nil {
		t.Fatalf("read user_portfolio provenance: %v", err)
	}
	if !portfolioSource.Valid || portfolioSource.String != "yahoo" {
		t.Errorf("user_portfolio price_source = %v, want yahoo", portfolioSource)
	}
	if !portfolioFetch.Valid || !portfolioFetch.Time.Equal(fetchedAt) {
		t.Errorf("user_portfolio price_fetched_at = %v, want %s", portfolioFetch, fetchedAt)
	}

	rewardID := insertHistoricalReward(t, db, userID, "MDHIST", 5, 3)
	rewardDate := scalarString(t, db, `SELECT reward_date::text FROM rewards WHERE id = $1`, rewardID)
	historyAt := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	historyFetched := historyAt.Add(30 * time.Second)
	mustExec(t, db, `
		INSERT INTO stock_price_history
			(stock_symbol, price, date, created_at, source, fetched_at,
			 provider_symbol, exchange, currency)
		VALUES
			('MDHIST', 450, $1::date, $2, 'nse_bhavcopy', $3, 'MDHIST', 'NSE', 'INR')
	`, rewardDate, historyAt, historyFetched)

	var (
		historyPrice      float64
		historyStatus     string
		historyAsOf       time.Time
		historySource     sql.NullString
		historyFetchedOut sql.NullTime
	)
	if err := db.QueryRow(`
		SELECT price::float8, price_status, price_as_of, price_source, price_fetched_at
		FROM historical_rewards_as_of(NULL::date)
		WHERE reward_event_id = $1
	`, rewardID).Scan(&historyPrice, &historyStatus, &historyAsOf, &historySource, &historyFetchedOut); err != nil {
		t.Fatalf("read historical_rewards_as_of provenance: %v", err)
	}
	if historyPrice != 450 || historyStatus != "available" {
		t.Errorf("historical price/status = %v/%q, want 450/available", historyPrice, historyStatus)
	}
	if !historyAsOf.Equal(historyAt) {
		t.Errorf("historical price_as_of = %s, want %s", historyAsOf, historyAt)
	}
	if !historySource.Valid || historySource.String != "nse_bhavcopy" {
		t.Errorf("historical price_source = %v, want nse_bhavcopy", historySource)
	}
	if !historyFetchedOut.Valid || !historyFetchedOut.Time.Equal(historyFetched) {
		t.Errorf("historical price_fetched_at = %v, want %s", historyFetchedOut, historyFetched)
	}

	// The historical_rewards view continues to be the grant-date wrapper.
	var viewSource string
	if err := db.QueryRow(`
		SELECT price_source
		FROM historical_rewards
		WHERE reward_event_id = $1
	`, rewardID).Scan(&viewSource); err != nil {
		t.Fatalf("read historical_rewards provenance: %v", err)
	}
	if viewSource != "nse_bhavcopy" {
		t.Errorf("historical_rewards price_source = %q, want nse_bhavcopy", viewSource)
	}
}

func assertCurrentPrice(
	t *testing.T,
	db *sql.DB,
	symbol, source string,
	price float64,
	updatedAt time.Time,
) {
	t.Helper()

	var (
		gotSource    string
		gotPrice     float64
		gotUpdatedAt time.Time
	)
	if err := db.QueryRow(`
		SELECT source, price::float8, updated_at
		FROM stock_prices
		WHERE stock_symbol = $1
	`, symbol).Scan(&gotSource, &gotPrice, &gotUpdatedAt); err != nil {
		t.Fatalf("read current price for %s: %v", symbol, err)
	}
	if gotSource != source {
		t.Errorf("current %s source = %q, want %q", symbol, gotSource, source)
	}
	if gotPrice != price {
		t.Errorf("current %s price = %v, want %v", symbol, gotPrice, price)
	}
	if !gotUpdatedAt.Equal(updatedAt) {
		t.Errorf("current %s updated_at = %s, want %s", symbol, gotUpdatedAt, updatedAt)
	}
}

func assertHistoryPrice(
	t *testing.T,
	db *sql.DB,
	symbol, date, source string,
	price float64,
	createdAt time.Time,
) {
	t.Helper()

	var (
		gotSource    string
		gotPrice     float64
		gotCreatedAt time.Time
	)
	if err := db.QueryRow(`
		SELECT source, price::float8, created_at
		FROM stock_price_history
		WHERE stock_symbol = $1 AND date = $2::date
	`, symbol, date).Scan(&gotSource, &gotPrice, &gotCreatedAt); err != nil {
		t.Fatalf("read history price for %s on %s: %v", symbol, date, err)
	}
	if gotSource != source {
		t.Errorf("history %s on %s source = %q, want %q", symbol, date, gotSource, source)
	}
	if gotPrice != price {
		t.Errorf("history %s on %s price = %v, want %v", symbol, date, gotPrice, price)
	}
	if !gotCreatedAt.Equal(createdAt) {
		t.Errorf("history %s on %s created_at = %s, want %s", symbol, date, gotCreatedAt, createdAt)
	}
}
