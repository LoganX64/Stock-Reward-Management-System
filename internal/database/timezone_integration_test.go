// Package database contains PostgreSQL integration tests for the business-date
// and timezone semantics introduced by migration 0022.
//
// These tests are opt-in because they create and drop a disposable database.
// Run them with, for example:
//
//	TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' go test ./internal/database -run Integration -v
//
// The URL must point at a database the test may connect to administratively;
// the test creates a uniquely named database and drops it afterwards. The
// session timezone of the server is intentionally irrelevant: every business
// date assertion holds under UTC, Asia/Kolkata, America/Los_Angeles, and
// Pacific/Kiritimati.
package database

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/lib/pq"
)

func TestBusinessDateSemanticsIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)

	t.Run("helpers ignore the session timezone", func(t *testing.T) {
		for _, zone := range []string{
			"UTC",
			"Asia/Kolkata",
			"America/Los_Angeles",
			"Pacific/Kiritimati",
		} {
			zone := zone
			t.Run(zone, func(t *testing.T) {
				withSessionZone(t, db, zone, func(tx *sql.Tx) {
					var (
						timezone, before, after string
						todayMatches            bool
					)
					if err := tx.QueryRow(`
						SELECT
							market_timezone(),
							market_date(TIMESTAMPTZ '2026-10-28 18:29:59+00')::text,
							market_date(TIMESTAMPTZ '2026-10-28 18:30:00+00')::text,
							market_today() = market_date(CURRENT_TIMESTAMP)
					`).Scan(&timezone, &before, &after, &todayMatches); err != nil {
						t.Fatalf("query market helpers: %v", err)
					}

					if timezone != "Asia/Kolkata" {
						t.Errorf("market_timezone() = %q, want Asia/Kolkata", timezone)
					}
					if before != "2026-10-28" {
						t.Errorf("18:29:59Z market date = %q, want 2026-10-28", before)
					}
					if after != "2026-10-29" {
						t.Errorf("18:30:00Z market date = %q, want 2026-10-29", after)
					}
					if !todayMatches {
						t.Error("market_today() must equal market_date(CURRENT_TIMESTAMP)")
					}
				})
			})
		}
	})

	t.Run("reward dates use the IST midnight boundary", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "reward-boundary")

		insertReward := func(symbol, createdAt string) string {
			t.Helper()
			var rewardDate string
			if err := db.QueryRow(`
				INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
				VALUES ($1, $2, 1, 1, $3::timestamptz)
				RETURNING reward_date::text
			`, userID, symbol, createdAt).Scan(&rewardDate); err != nil {
				t.Fatalf("insert reward %s at %s: %v", symbol, createdAt, err)
			}
			return rewardDate
		}

		// 23:59:59 IST is still the 28th; one second later is the 29th.
		if got := insertReward("TZREW", "2026-10-28 18:29:59+00"); got != "2026-10-28" {
			t.Errorf("reward at 18:29:59Z has reward_date %q, want 2026-10-28", got)
		}
		if got := insertReward("TZREW", "2026-10-28 18:30:00+00"); got != "2026-10-29" {
			t.Errorf("reward at 18:30:00Z has reward_date %q, want 2026-10-29", got)
		}

		// The trigger overrides an explicitly supplied reward_date.
		var overridden string
		if err := db.QueryRow(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at, reward_date)
			VALUES ($1, 'TZINF', 1, 1, TIMESTAMPTZ '2026-10-27 18:30:00+00', DATE '1999-01-01')
			RETURNING reward_date::text
		`, userID).Scan(&overridden); err != nil {
			t.Fatalf("insert reward with explicit reward_date: %v", err)
		}
		if overridden != "2026-10-28" {
			t.Errorf("trigger-derived reward_date = %q, want 2026-10-28", overridden)
		}

		// 20:00Z is 01:30 IST on the 29th, so it collides with the second
		// TZREW reward above.
		_, err := db.Exec(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
			VALUES ($1, 'TZREW', 1, 1, TIMESTAMPTZ '2026-10-28 20:00:00+00')
		`, userID)
		assertUniqueViolation(t, err, "same-day reward uniqueness")

		// Updating created_at re-derives reward_date: 18:30Z on the 26th is
		// midnight IST on the 27th.
		var updated string
		if err := db.QueryRow(`
			UPDATE rewards
			SET created_at = TIMESTAMPTZ '2026-10-26 18:30:00+00'
			WHERE user_id = $1 AND stock_symbol = 'TZINF'
			RETURNING reward_date::text
		`, userID).Scan(&updated); err != nil {
			t.Fatalf("update reward created_at: %v", err)
		}
		if updated != "2026-10-27" {
			t.Errorf("updated reward_date = %q, want 2026-10-27", updated)
		}

		// A reward created with the column default lands on market_today().
		var (
			defaultDate string
			marketToday string
		)
		if err := db.QueryRow(`SELECT market_today()::text`).Scan(&marketToday); err != nil {
			t.Fatalf("read market_today: %v", err)
		}
		if err := db.QueryRow(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity)
			VALUES ($1, 'TZDEF', 1, 1)
			RETURNING reward_date::text
		`, userID).Scan(&defaultDate); err != nil {
			t.Fatalf("insert default-dated reward: %v", err)
		}
		if defaultDate != marketToday {
			t.Errorf("default reward_date = %q, want market_today() %q", defaultDate, marketToday)
		}

		if got := columnType(t, db, "rewards", "created_at"); got != "timestamp with time zone" {
			t.Errorf("rewards.created_at type = %q, want timestamp with time zone", got)
		}
		if got := columnType(t, db, "rewards", "reward_date"); got != "date" {
			t.Errorf("rewards.reward_date type = %q, want date", got)
		}
		var generated string
		if err := db.QueryRow(`
			SELECT is_generated
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'rewards' AND column_name = 'reward_date'
		`).Scan(&generated); err != nil {
			t.Fatalf("read reward_date generation flag: %v", err)
		}
		if generated != "NEVER" {
			t.Errorf("rewards.reward_date is_generated = %q, want NEVER", generated)
		}
	})

	t.Run("corporate action boundaries are inclusive market dates", func(t *testing.T) {
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES ('TZSPLIT', 'split', 2, 1, DATE '2026-11-01')
		`)

		var before, on float64
		if err := db.QueryRow(`
			SELECT
				stock_action_factor('TZSPLIT', DATE '2026-10-31')::float8,
				stock_action_factor('TZSPLIT', DATE '2026-11-01')::float8
		`).Scan(&before, &on); err != nil {
			t.Fatalf("read split factors: %v", err)
		}
		if before != 1 {
			t.Errorf("split factor before effective date = %v, want 1", before)
		}
		if on != 2 {
			t.Errorf("split factor on effective date = %v, want 2", on)
		}

		var delistEventID int
		if err := db.QueryRow(`
			INSERT INTO stock_events (
				stock_symbol, event_type, ratio_num, ratio_den, effective_date,
				delist_outcome, cash_per_unit
			)
			VALUES ('TZDELIST', 'delist', 1, 1, DATE '2026-12-01', 'cash_exit', 100)
			RETURNING id
		`).Scan(&delistEventID); err != nil {
			t.Fatalf("insert delist event: %v", err)
		}

		var delistedBefore, delistedOn bool
		if err := db.QueryRow(`
			SELECT
				EXISTS (
					SELECT 1
					FROM instrument_delist_event('TZDELIST', DATE '2026-01-01', DATE '2026-11-30')
				),
				EXISTS (
					SELECT 1
					FROM instrument_delist_event('TZDELIST', DATE '2026-01-01', DATE '2026-12-01')
				)
		`).Scan(&delistedBefore, &delistedOn); err != nil {
			t.Fatalf("read delist presence: %v", err)
		}
		if delistedBefore {
			t.Error("delist is visible before its effective date")
		}
		if !delistedOn {
			t.Error("delist is not visible on its effective date")
		}

		var outcome string
		var cashPerUnit float64
		if err := db.QueryRow(`
			SELECT outcome, cash_per_unit::float8
			FROM instrument_delist_event('TZDELIST', DATE '2026-01-01', DATE '2026-12-01')
			WHERE event_id = $1
		`, delistEventID).Scan(&outcome, &cashPerUnit); err != nil {
			t.Fatalf("read delist outcome: %v", err)
		}
		if outcome != "cash_exit" || cashPerUnit != 100 {
			t.Errorf("delist outcome = %q/%v, want cash_exit/100", outcome, cashPerUnit)
		}

		if got := columnType(t, db, "stock_events", "effective_date"); got != "date" {
			t.Errorf("stock_events.effective_date type = %q, want date", got)
		}
	})

	t.Run("price history uses market_today", func(t *testing.T) {
		mustExec(t, db, `
			INSERT INTO stock_prices (stock_symbol, price)
			VALUES ('TZPX', 123.45)
			ON CONFLICT (stock_symbol) DO UPDATE SET price = EXCLUDED.price
		`)

		var inserted int
		var marketToday string
		if err := db.QueryRow(`
			SELECT backfill_price_history(market_today()), market_today()::text
		`).Scan(&inserted, &marketToday); err != nil {
			t.Fatalf("backfill price history: %v", err)
		}
		if inserted < 1 {
			t.Fatalf("backfill inserted %d rows, want at least 1 for TZPX", inserted)
		}

		var historyDate string
		if err := db.QueryRow(`
			SELECT date::text
			FROM stock_price_history
			WHERE stock_symbol = 'TZPX'
		`).Scan(&historyDate); err != nil {
			t.Fatalf("read price history date: %v", err)
		}
		if historyDate != marketToday {
			t.Errorf("price history date = %q, want market_today() %q", historyDate, marketToday)
		}

		var secondRun int
		if err := db.QueryRow(`SELECT backfill_price_history(market_today())`).Scan(&secondRun); err != nil {
			t.Fatalf("second backfill: %v", err)
		}
		if secondRun != 0 {
			t.Errorf("second backfill inserted %d rows, want 0", secondRun)
		}

		if got := columnType(t, db, "stock_price_history", "date"); got != "date" {
			t.Errorf("stock_price_history.date type = %q, want date", got)
		}
		if got := columnType(t, db, "stock_price_history", "created_at"); got != "timestamp with time zone" {
			t.Errorf("stock_price_history.created_at type = %q, want timestamp with time zone", got)
		}
	})

	t.Run("historical valuation follows market dates and returns absolute instants", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "historical-dates")

		var rewardID int
		if err := db.QueryRow(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
			VALUES ($1, 'TZVAL', 2, 2, TIMESTAMPTZ '2026-10-27 18:30:00+00')
			RETURNING id
		`, userID).Scan(&rewardID); err != nil {
			t.Fatalf("insert historical reward: %v", err)
		}

		mustExec(t, db, `
			INSERT INTO stock_price_history (stock_symbol, price, date, created_at)
			VALUES
				('TZVAL', 100, DATE '2026-10-28', TIMESTAMPTZ '2026-10-28 09:00:00+05:30'),
				('TZVAL', 110, DATE '2026-10-29', TIMESTAMPTZ '2026-10-29 09:00:00+05:30')
		`)

		var (
			symbol      string
			quantity    float64
			price       float64
			priceStatus string
			priceAsOf   time.Time
		)
		if err := db.QueryRow(`
			SELECT stock_symbol, adjusted_quantity::float8, price::float8, price_status, price_as_of
			FROM historical_rewards_as_of(NULL::date)
			WHERE reward_event_id = $1
		`, rewardID).Scan(&symbol, &quantity, &price, &priceStatus, &priceAsOf); err != nil {
			t.Fatalf("read grant-date valuation: %v", err)
		}
		if symbol != "TZVAL" {
			t.Errorf("grant-date symbol = %q, want TZVAL", symbol)
		}
		if quantity != 2 {
			t.Errorf("grant-date quantity = %v, want 2", quantity)
		}
		if price != 100 {
			t.Errorf("grant-date price = %v, want 100", price)
		}
		if priceStatus != "available" {
			t.Errorf("grant-date price_status = %q, want available", priceStatus)
		}
		wantAsOf := time.Date(2026, 10, 28, 3, 30, 0, 0, time.UTC)
		if !priceAsOf.UTC().Equal(wantAsOf) {
			t.Errorf("price_as_of = %s, want %s", priceAsOf.UTC(), wantAsOf)
		}

		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, price::float8, price_status
			FROM historical_rewards_as_of(DATE '2026-10-29')
			WHERE reward_event_id = $1
		`, rewardID).Scan(&quantity, &price, &priceStatus); err != nil {
			t.Fatalf("read as-of valuation: %v", err)
		}
		if quantity != 2 || price != 110 || priceStatus != "available" {
			t.Errorf("as-of valuation = qty %v price %v status %q, want 2/110/available", quantity, price, priceStatus)
		}

		var beforeGrant int
		if err := db.QueryRow(`
			SELECT count(*)
			FROM historical_rewards_as_of(DATE '2026-10-27')
			WHERE reward_event_id = $1
		`, rewardID).Scan(&beforeGrant); err != nil {
			t.Fatalf("read pre-grant valuation: %v", err)
		}
		if beforeGrant != 0 {
			t.Errorf("historical row appears before its grant date: count = %d", beforeGrant)
		}

		var returnType string
		if err := db.QueryRow(`
			SELECT pg_get_function_result('historical_rewards_as_of(date)'::regprocedure)
		`).Scan(&returnType); err != nil {
			t.Fatalf("read historical function result type: %v", err)
		}
		if !strings.Contains(returnType, "price_as_of timestamp with time zone") {
			t.Errorf("historical_rewards_as_of price_as_of is not TIMESTAMPTZ: %s", returnType)
		}
	})

	t.Run("corporate action cash is idempotent on the market date", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "corporate-cash")

		var rewardID int
		if err := db.QueryRow(`
			INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
			VALUES ($1, 'TZCASH', 1, 1, TIMESTAMPTZ '2020-01-01 10:00:00+05:30')
			RETURNING id
		`, userID).Scan(&rewardID); err != nil {
			t.Fatalf("insert cash reward: %v", err)
		}

		var eventID int
		if err := db.QueryRow(`
			INSERT INTO stock_events (
				stock_symbol, event_type, ratio_num, ratio_den, effective_date,
				fraction_policy, fraction_cash_per_unit
			)
			VALUES ('TZCASH', 'split', 3, 2, market_today(), 'cash_in_lieu', 100)
			RETURNING id
		`).Scan(&eventID); err != nil {
			t.Fatalf("insert cash-in-lieu split: %v", err)
		}

		assertCashPosting := func(wantAmount float64) {
			t.Helper()
			var (
				count       int
				amount      float64
				entryType   string
				postedEvent int
			)
			if err := db.QueryRow(`
				SELECT count(*), COALESCE(MAX(amount), 0)::float8,
				       COALESCE(MAX(entry_type::text), ''), COALESCE(MAX(stock_event_id), 0)
				FROM ledger
				WHERE reward_id = $1 AND stock_event_id IS NOT NULL
			`, rewardID).Scan(&count, &amount, &entryType, &postedEvent); err != nil {
				t.Fatalf("read cash posting: %v", err)
			}
			if count != 1 {
				t.Fatalf("cash posting count = %d, want 1", count)
			}
			if amount != wantAmount {
				t.Errorf("cash posting amount = %v, want %v", amount, wantAmount)
			}
			if entryType != "inr_outflow" {
				t.Errorf("cash posting entry_type = %q, want inr_outflow", entryType)
			}
			if postedEvent != eventID {
				t.Errorf("cash posting stock_event_id = %d, want %d", postedEvent, eventID)
			}
		}

		// The stock-event trigger processes the effective event immediately.
		assertCashPosting(50)

		var postings int
		if err := db.QueryRow(`SELECT process_corporate_action_cash()`).Scan(&postings); err != nil {
			t.Fatalf("process corporate action cash: %v", err)
		}
		if postings != 1 {
			t.Errorf("default-argument postings = %d, want 1", postings)
		}
		assertCashPosting(50)

		// The day before the effective date has no cash posting; the existing
		// row is removed by the reconciliation delete.
		if err := db.QueryRow(`SELECT process_corporate_action_cash(market_today() - 1)`).Scan(&postings); err != nil {
			t.Fatalf("process corporate action cash before effective date: %v", err)
		}
		var count int
		if err := db.QueryRow(`
			SELECT count(*) FROM ledger WHERE reward_id = $1 AND stock_event_id IS NOT NULL
		`, rewardID).Scan(&count); err != nil {
			t.Fatalf("read pre-effective cash posting: %v", err)
		}
		if count != 0 {
			t.Errorf("cash posting exists before the effective date: count = %d", count)
		}

		// Processing on the market date restores it.
		if err := db.QueryRow(`SELECT process_corporate_action_cash(market_today())`).Scan(&postings); err != nil {
			t.Fatalf("reprocess corporate action cash: %v", err)
		}
		assertCashPosting(50)

		var cashInLieu, adjustedQuantity float64
		if err := db.QueryRow(`
			SELECT cash_in_lieu_proceeds::float8, adjusted_quantity::float8
			FROM reward_corporate_action_outcome($1, market_today())
		`, rewardID).Scan(&cashInLieu, &adjustedQuantity); err != nil {
			t.Fatalf("read corporate action outcome: %v", err)
		}
		if cashInLieu != 50 {
			t.Errorf("cash_in_lieu_proceeds = %v, want 50", cashInLieu)
		}
		if adjustedQuantity != 1 {
			t.Errorf("adjusted_quantity = %v, want 1", adjustedQuantity)
		}
	})

	t.Run("audit columns use timestamptz and business dates use date", func(t *testing.T) {
		timestampColumns := []struct {
			table  string
			column string
		}{
			{"users", "created_at"},
			{"rewards", "created_at"},
			{"ledger", "created_at"},
			{"adjustments", "created_at"},
			{"stock_prices", "updated_at"},
			{"stock_price_history", "created_at"},
		}
		for _, column := range timestampColumns {
			if got := columnType(t, db, column.table, column.column); got != "timestamp with time zone" {
				t.Errorf("%s.%s type = %q, want timestamp with time zone", column.table, column.column, got)
			}
		}

		dateColumns := []struct {
			table  string
			column string
		}{
			{"rewards", "reward_date"},
			{"stock_events", "effective_date"},
			{"adjustments", "effective_date"},
			{"stock_price_history", "date"},
		}
		for _, column := range dateColumns {
			if got := columnType(t, db, column.table, column.column); got != "date" {
				t.Errorf("%s.%s type = %q, want date", column.table, column.column, got)
			}
		}
	})
}

// TestLegacyTimestampMigrationIntegration verifies the one-way data conversion
// performed by migration 0022: legacy TIMESTAMP values are interpreted as IST
// wall-clock time, and every existing reward_date is preserved.
func TestLegacyTimestampMigrationIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, 21)

	db := openIntegrationDB(t, dbURL)

	var userID int
	if err := db.QueryRow(`
		INSERT INTO users (username, email)
		VALUES ('legacy-tz', 'legacy-tz@example.com')
		RETURNING id
	`).Scan(&userID); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}

	var rewardID int
	if err := db.QueryRow(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
		VALUES ($1, 'LEGACY', 1, 1, TIMESTAMP '2026-10-28 23:59:59')
		RETURNING id
	`, userID).Scan(&rewardID); err != nil {
		t.Fatalf("insert legacy reward: %v", err)
	}

	var legacyRewardDate string
	if err := db.QueryRow(`SELECT reward_date::text FROM rewards WHERE id = $1`, rewardID).Scan(&legacyRewardDate); err != nil {
		t.Fatalf("read legacy reward_date: %v", err)
	}
	if legacyRewardDate != "2026-10-28" {
		t.Fatalf("legacy reward_date = %q, want 2026-10-28", legacyRewardDate)
	}

	var legacyEventID int
	if err := db.QueryRow(`
		INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
		VALUES ('LEGACYEV', 'split', 2, 1, TIMESTAMP '2026-10-20 15:30:00')
		RETURNING id
	`).Scan(&legacyEventID); err != nil {
		t.Fatalf("insert legacy stock event: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration database connection: %v", err)
	}

	applyMigrations(t, dbURL, -1)

	db = openIntegrationDB(t, dbURL)

	var (
		rewardDate     string
		createdAtIsIst bool
		eventEffective string
		eventIDAfter   int
	)
	if err := db.QueryRow(`
		SELECT
			reward_date::text,
			created_at = TIMESTAMPTZ '2026-10-28 23:59:59+05:30'
		FROM rewards
		WHERE id = $1
	`, rewardID).Scan(&rewardDate, &createdAtIsIst); err != nil {
		t.Fatalf("read migrated reward: %v", err)
	}
	if rewardDate != "2026-10-28" {
		t.Errorf("migrated reward_date = %q, want preserved 2026-10-28", rewardDate)
	}
	if !createdAtIsIst {
		t.Error("legacy created_at was not reinterpreted as Asia/Kolkata wall-clock time")
	}

	if err := db.QueryRow(`SELECT effective_date::text, id FROM stock_events WHERE id = $1`, legacyEventID).
		Scan(&eventEffective, &eventIDAfter); err != nil {
		t.Fatalf("read migrated stock event: %v", err)
	}
	if eventEffective != "2026-10-20" {
		t.Errorf("migrated effective_date = %q, want 2026-10-20", eventEffective)
	}
	if eventIDAfter != legacyEventID {
		t.Errorf("migrated stock event id = %d, want %d", eventIDAfter, legacyEventID)
	}

	if got := columnType(t, db, "stock_events", "effective_date"); got != "date" {
		t.Errorf("migrated stock_events.effective_date type = %q, want date", got)
	}
	if got := columnType(t, db, "rewards", "created_at"); got != "timestamp with time zone" {
		t.Errorf("migrated rewards.created_at type = %q, want timestamp with time zone", got)
	}

	// New rewards written after the migration keep using the same IST rule.
	var newRewardDate string
	if err := db.QueryRow(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
		VALUES ($1, 'LEGACY2', 1, 1, TIMESTAMPTZ '2026-10-28 18:30:00+00')
		RETURNING reward_date::text
	`, userID).Scan(&newRewardDate); err != nil {
		t.Fatalf("insert post-migration reward: %v", err)
	}
	if newRewardDate != "2026-10-29" {
		t.Errorf("post-migration reward_date = %q, want 2026-10-29", newRewardDate)
	}
}

func integrationDatabaseURL(t *testing.T) string {
	t.Helper()
	value := os.Getenv("TEST_DATABASE_URL")
	if value == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}
	return value
}

// createIntegrationDatabase creates a uniquely named disposable database and
// registers its removal. Cleanup order is LIFO: the test's own pool closes
// before the database is dropped.
func createIntegrationDatabase(t *testing.T, adminURL string) string {
	t.Helper()

	admin, err := sql.Open("postgres", adminURL)
	if err != nil {
		t.Fatalf("open administrative database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	if err := admin.Ping(); err != nil {
		t.Fatalf("ping administrative database: %v", err)
	}

	name := fmt.Sprintf("stocky_tz_it_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`); err != nil {
			t.Logf("cleanup: failed to drop test database %s: %v", name, err)
		}
	})

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// applyMigrations applies migrations up to version, or all migrations when
// version is -1.
func applyMigrations(t *testing.T, dbURL string, version int) {
	t.Helper()

	m, err := migrate.New("file://./migrations", dbURL)
	if err != nil {
		t.Fatalf("create migrate instance: %v", err)
	}

	if version < 0 {
		err = m.Up()
	} else {
		err = m.Migrate(uint(version))
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		srcErr, dbErr := m.Close()
		t.Fatalf("apply migrations (version %d): %v (source close: %v, database close: %v)", version, err, srcErr, dbErr)
	}

	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Logf("close migrate instance: source=%v database=%v", srcErr, dbErr)
	}
}

func openIntegrationDB(t *testing.T, dbURL string) *sql.DB {
	t.Helper()

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(); err != nil {
		t.Fatalf("ping test database: %v", err)
	}
	return db
}

func withSessionZone(t *testing.T, db *sql.DB, zone string, fn func(*sql.Tx)) {
	t.Helper()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin zone transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`SET LOCAL TIME ZONE ` + pq.QuoteLiteral(zone)); err != nil {
		t.Fatalf("set session zone %s: %v", zone, err)
	}

	fn(tx)
}

func insertIntegrationUser(t *testing.T, db *sql.DB, name string) int {
	t.Helper()

	var id int
	if err := db.QueryRow(`
		INSERT INTO users (username, email)
		VALUES ($1, $2)
		RETURNING id
	`, name, name+"@example.com").Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", name, err)
	}
	return id
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec failed: %v\nquery: %s", err, query)
	}
}

func columnType(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()

	var dataType string
	if err := db.QueryRow(`
		SELECT data_type
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
	`, table, column).Scan(&dataType); err != nil {
		t.Fatalf("read type of %s.%s: %v", table, column, err)
	}
	return dataType
}

func assertUniqueViolation(t *testing.T, err error, context string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s: expected a unique violation, got nil", context)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("%s: expected *pq.Error, got %T: %v", context, err, err)
	}
	if pqErr.Code != "23505" {
		t.Fatalf("%s: expected SQLSTATE 23505, got %s: %v", context, pqErr.Code, err)
	}
}
