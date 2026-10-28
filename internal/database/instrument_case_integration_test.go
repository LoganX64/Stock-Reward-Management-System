// PostgreSQL integration tests for case normalization across instruments.
//
// These tests are opt-in like the business-date integration tests. Run them
// with, for example:
//
//	TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' go test ./internal/database -run InstrumentCase -v
package database

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func TestInstrumentCaseNormalizationIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, 22)

	db := openIntegrationDB(t, dbURL)
	userID := insertIntegrationUser(t, db, "case-main")

	var rewardID int
	if err := db.QueryRow(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
		VALUES ($1, ' tcs ', 2, 2, TIMESTAMPTZ '2026-10-28 06:00:00+00')
		RETURNING id
	`, userID).Scan(&rewardID); err != nil {
		t.Fatalf("seed lowercase reward: %v", err)
	}

	var eventID int
	if err := db.QueryRow(`
		INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
		VALUES (' tcs ', 'split', 2, 1, DATE '2026-10-20')
		RETURNING id
	`).Scan(&eventID); err != nil {
		t.Fatalf("seed lowercase stock event: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount)
		VALUES ($1, 'stock_units', ' tcs ', 2, 0)
	`, rewardID); err != nil {
		t.Fatalf("seed lowercase ledger symbol: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO stock_prices (stock_symbol, price, updated_at)
		VALUES
			('AAPL', 100, TIMESTAMPTZ '2026-10-01 10:00:00+00'),
			('aapl', 200, TIMESTAMPTZ '2026-10-02 10:00:00+00'),
			(' aapl ', 150, TIMESTAMPTZ '2026-10-01 12:00:00+00')
	`); err != nil {
		t.Fatalf("seed case-variant prices: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO stock_price_history (stock_symbol, price, date, created_at)
		VALUES
			('AAPL', 100, DATE '2026-10-01', TIMESTAMPTZ '2026-10-01 10:00:00+00'),
			('aapl', 200, DATE '2026-10-01', TIMESTAMPTZ '2026-10-02 10:00:00+00'),
			(' aapl ', 150, DATE '2026-10-01', TIMESTAMPTZ '2026-10-01 12:00:00+00')
	`); err != nil {
		t.Fatalf("seed case-variant price history: %v", err)
	}

	applyMigrations(t, dbURL, 23)

	t.Run("normalization helper", func(t *testing.T) {
		var got string
		if err := db.QueryRow(`SELECT normalize_instrument_symbol(E'\t tcs \r\n')`).Scan(&got); err != nil {
			t.Fatalf("call normalize_instrument_symbol: %v", err)
		}
		if got != "TCS" {
			t.Errorf("normalize_instrument_symbol = %q, want TCS", got)
		}
	})

	t.Run("deduplicates current prices by latest update", func(t *testing.T) {
		var count int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM stock_prices
			WHERE stock_symbol = 'AAPL'
		`).Scan(&count); err != nil {
			t.Fatalf("count AAPL stock_prices: %v", err)
		}
		if count != 1 {
			t.Fatalf("AAPL stock_prices row count = %d, want 1", count)
		}

		var price float64
		if err := db.QueryRow(`
			SELECT price
			FROM stock_prices
			WHERE stock_symbol = 'AAPL'
		`).Scan(&price); err != nil {
			t.Fatalf("read stock price: %v", err)
		}
		if price != 200 {
			t.Errorf("stock_prices price = %v, want 200 (latest updated_at)", price)
		}
	})

	t.Run("deduplicates history by latest write", func(t *testing.T) {
		var count int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM stock_price_history
			WHERE date = DATE '2026-10-01'
		`).Scan(&count); err != nil {
			t.Fatalf("count stock_price_history: %v", err)
		}
		if count != 1 {
			t.Fatalf("stock_price_history row count = %d, want 1", count)
		}

		var symbol string
		var price float64
		if err := db.QueryRow(`
			SELECT stock_symbol, price
			FROM stock_price_history
			WHERE date = DATE '2026-10-01'
		`).Scan(&symbol, &price); err != nil {
			t.Fatalf("read stock price history: %v", err)
		}
		if symbol != "AAPL" {
			t.Errorf("stock_price_history symbol = %q, want AAPL", symbol)
		}
		if price != 200 {
			t.Errorf("stock_price_history price = %v, want 200 (latest created_at)", price)
		}
	})

	t.Run("normalizes existing rewards and events", func(t *testing.T) {
		var rewardSymbol string
		if err := db.QueryRow(`SELECT stock_symbol FROM rewards WHERE id = $1`, rewardID).Scan(&rewardSymbol); err != nil {
			t.Fatalf("read reward symbol: %v", err)
		}
		if rewardSymbol != "TCS" {
			t.Errorf("reward symbol = %q, want TCS", rewardSymbol)
		}

		var eventSymbol string
		if err := db.QueryRow(`SELECT stock_symbol FROM stock_events WHERE id = $1`, eventID).Scan(&eventSymbol); err != nil {
			t.Fatalf("read event symbol: %v", err)
		}
		if eventSymbol != "TCS" {
			t.Errorf("stock event symbol = %q, want TCS", eventSymbol)
		}

		var ledgerSymbol string
		if err := db.QueryRow(`
			SELECT stock_symbol
			FROM ledger
			WHERE reward_id = $1 AND entry_type = 'stock_units'
		`, rewardID).Scan(&ledgerSymbol); err != nil {
			t.Fatalf("read ledger symbol: %v", err)
		}
		if ledgerSymbol != "TCS" {
			t.Errorf("ledger symbol = %q, want TCS", ledgerSymbol)
		}

		if _, err := db.Exec(`
			INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount)
			VALUES ($1, 'stock_units', 'tcs', 1, 0)
		`, rewardID); err != nil {
			t.Fatalf("insert lowercase ledger symbol: %v", err)
		}
		if err := db.QueryRow(`
			SELECT stock_symbol
			FROM ledger
			WHERE reward_id = $1
			ORDER BY id DESC
			LIMIT 1
		`, rewardID).Scan(&ledgerSymbol); err != nil {
			t.Fatalf("read inserted ledger symbol: %v", err)
		}
		if ledgerSymbol != "TCS" {
			t.Errorf("new ledger symbol = %q, want TCS", ledgerSymbol)
		}
	})

	t.Run("canonical constraints and triggers are installed", func(t *testing.T) {
		for _, constraint := range []string{
			"stock_prices_symbol_canonical",
			"stock_price_history_symbol_canonical",
			"rewards_symbol_canonical",
			"stock_events_symbol_canonical",
			"stock_events_new_symbol_canonical",
			"ledger_symbol_canonical",
		} {
			if !constraintExists(t, db, constraint) {
				t.Errorf("constraint %s is missing", constraint)
			}
		}

		for _, trigger := range []string{
			"trg_00_normalize_stock_price_symbol",
			"trg_00_normalize_stock_price_history_symbol",
			"trg_00_normalize_reward_symbol",
			"trg_00_normalize_ledger_symbol",
			"trg_00_normalize_stock_event",
		} {
			if !triggerExists(t, db, trigger) {
				t.Errorf("trigger %s is missing", trigger)
			}
		}
	})

	t.Run("rejects case variants through normalized uniqueness", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO stock_prices (stock_symbol, price) VALUES (' aapl ', 300)`)
		assertUniqueViolation(t, err, "case-variant stock price")

		_, err = db.Exec(`
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES (' aapl ', 300, DATE '2026-10-01')
		`)
		assertUniqueViolation(t, err, "case-variant stock price history")

		_, err = db.Exec(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
			VALUES ($1, 'TCS', 1, 1, TIMESTAMPTZ '2026-10-28 06:00:00+00')
		`, userID)
		assertUniqueViolation(t, err, "case-variant reward")

		_, err = db.Exec(`
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES (' tcs ', 'split', 2, 1, DATE '2026-10-20')
		`)
		assertUniqueViolation(t, err, "case-variant stock event")
	})

	t.Run("exact conflict targets upsert normalized rows", func(t *testing.T) {
		if _, err := db.Exec(`
			INSERT INTO stock_prices (stock_symbol, price)
			VALUES (' msft ', 300)
			ON CONFLICT (stock_symbol) DO UPDATE SET price = EXCLUDED.price
		`); err != nil {
			t.Fatalf("upsert stock price through the exact conflict target: %v", err)
		}

		var symbol string
		var price float64
		if err := db.QueryRow(`
			SELECT stock_symbol, price
			FROM stock_prices
			WHERE stock_symbol = 'MSFT'
		`).Scan(&symbol, &price); err != nil {
			t.Fatalf("read upserted stock price: %v", err)
		}
		if symbol != "MSFT" {
			t.Errorf("upserted stock price symbol = %q, want MSFT", symbol)
		}
		if price != 300 {
			t.Errorf("upserted stock price = %v, want 300", price)
		}

		if _, err := db.Exec(`
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES (' msft ', 310, DATE '2026-10-28')
			ON CONFLICT (stock_symbol, date) DO UPDATE SET price = EXCLUDED.price
		`); err != nil {
			t.Fatalf("upsert price history through the exact conflict target: %v", err)
		}

		if err := db.QueryRow(`
			SELECT stock_symbol, price
			FROM stock_price_history
			WHERE stock_symbol = 'MSFT' AND date = DATE '2026-10-28'
		`).Scan(&symbol, &price); err != nil {
			t.Fatalf("read upserted price history: %v", err)
		}
		if symbol != "MSFT" {
			t.Errorf("upserted price history symbol = %q, want MSFT", symbol)
		}
		if price != 310 {
			t.Errorf("upserted price history = %v, want 310", price)
		}
	})

	t.Run("valuations are not multiplied", func(t *testing.T) {
		valuationUserID := insertIntegrationUser(t, db, "case-valuation")
		if _, err := db.Exec(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity)
			VALUES ($1, ' aapl ', 2, 2)
		`, valuationUserID); err != nil {
			t.Fatalf("insert valuation reward: %v", err)
		}

		var todayCount int
		var todayValue sql.NullFloat64
		if err := db.QueryRow(`
			SELECT COUNT(*), SUM(inr_value)
			FROM today_rewards
			WHERE user_id = $1
		`, valuationUserID).Scan(&todayCount, &todayValue); err != nil {
			t.Fatalf("read today_rewards: %v", err)
		}
		if todayCount != 1 {
			t.Errorf("today_rewards row count = %d, want 1", todayCount)
		}
		if !todayValue.Valid || todayValue.Float64 != 400 {
			t.Errorf("today_rewards value = %v, want 400", todayValue)
		}

		var portfolioCount int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM user_portfolio
			WHERE user_id = $1
		`, valuationUserID).Scan(&portfolioCount); err != nil {
			t.Fatalf("read user_portfolio: %v", err)
		}
		if portfolioCount != 1 {
			t.Errorf("user_portfolio row count = %d, want 1", portfolioCount)
		}
	})

	t.Run("down migration restores the previous schema without changing data", func(t *testing.T) {
		applyMigrations(t, dbURL, 22)

		if indexExists(t, db, "unique_stock_price_symbol_normalized") {
			t.Error("unique_stock_price_symbol_normalized still exists after down migration")
		}
		if indexExists(t, db, "unique_user_stock_date_normalized") {
			t.Error("unique_user_stock_date_normalized still exists after down migration")
		}
		if !indexExists(t, db, "unique_user_stock_date_upper") {
			t.Error("unique_user_stock_date_upper was not restored by the down migration")
		}
		if triggerExists(t, db, "trg_00_normalize_stock_price_symbol") {
			t.Error("stock price normalization trigger still exists after down migration")
		}
		if functionExists(t, db, "normalize_instrument_symbol") {
			t.Error("normalize_instrument_symbol still exists after down migration")
		}

		var symbol string
		if err := db.QueryRow(`
			SELECT stock_symbol
			FROM stock_prices
			WHERE stock_symbol = 'AAPL'
		`).Scan(&symbol); err != nil {
			t.Fatalf("read stock price after down migration: %v", err)
		}
		if symbol != "AAPL" {
			t.Errorf("stock price symbol after down migration = %q, want AAPL", symbol)
		}

		applyMigrations(t, dbURL, 23)

		if !indexExists(t, db, "unique_stock_price_symbol_normalized") {
			t.Error("unique_stock_price_symbol_normalized was not restored by the up migration")
		}
		if !triggerExists(t, db, "trg_00_normalize_stock_price_symbol") {
			t.Error("stock price normalization trigger was not restored by the up migration")
		}
		if !functionExists(t, db, "normalize_instrument_symbol") {
			t.Error("normalize_instrument_symbol was not restored by the up migration")
		}
	})
}

func TestInstrumentCaseNormalizationAbortsOnRewardDuplicatesIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, 22)

	db := openIntegrationDB(t, dbURL)
	userID := insertIntegrationUser(t, db, "case-abort")

	if _, err := db.Exec(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
		VALUES
			($1, 'DUP', 1, 1, TIMESTAMPTZ '2026-10-28 06:00:00+00'),
			($1, ' dup ', 1, 1, TIMESTAMPTZ '2026-10-28 06:00:00+00')
	`, userID); err != nil {
		t.Fatalf("seed whitespace reward variants: %v", err)
	}

	m, err := migrate.New("file://./migrations", dbURL)
	if err != nil {
		t.Fatalf("create migrate instance: %v", err)
	}

	err = m.Migrate(23)
	if err == nil {
		_, _ = m.Close()
		t.Fatal("expected migration to abort on normalized reward duplicates")
	}
	if !strings.Contains(err.Error(), "Cannot normalize rewards") {
		t.Errorf("migration error = %v, want a normalized reward duplicate message", err)
	}
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Logf("close migrate instance: source=%v database=%v", srcErr, dbErr)
	}
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("check index %s: %v", name, err)
	}
	return reg.Valid
}

func triggerExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM pg_trigger
		WHERE tgname = $1
		  AND NOT tgisinternal
	`, name).Scan(&count); err != nil {
		t.Fatalf("check trigger %s: %v", name, err)
	}
	return count > 0
}

func functionExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM pg_proc
		WHERE proname = $1
	`, name).Scan(&count); err != nil {
		t.Fatalf("check function %s: %v", name, err)
	}
	return count > 0
}

func constraintExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE c.conname = $1
		  AND n.nspname = 'public'
	`, name).Scan(&count); err != nil {
		t.Fatalf("check constraint %s: %v", name, err)
	}
	return count > 0
}
