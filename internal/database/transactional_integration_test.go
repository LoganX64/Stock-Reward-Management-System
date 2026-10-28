// PostgreSQL integration tests for the transactional and valuation behavior
// described in Phase 4.18.
//
// These tests are opt-in like the existing business-date and case-normalization
// integration tests. Run them with:
//
//	TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' \
//	  go test ./internal/database -run Integration -v -count=1
//
// Every top-level test creates and drops its own disposable database, so the
// suite does not depend on the schema of the database named in TEST_DATABASE_URL.
package database

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/LoganX64/stocky-api/internal/handlers/stocky"
	"github.com/gin-gonic/gin"
)

// TestCleanMigrationIntegration verifies that a completely empty database can
// be migrated to the latest version and that the objects the API depends on
// exist afterwards. Running the migration again must be a no-op.
func TestCleanMigrationIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)

	var (
		version int64
		dirty   bool
	)
	if err := db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != 23 {
		t.Errorf("migration version = %d, want 23", version)
	}
	if dirty {
		t.Error("schema_migrations is dirty after a clean migration")
	}

	for _, view := range []string{
		"today_rewards",
		"user_portfolio",
		"historical_rewards",
		"reward_corporate_action_outcomes",
		"user_corporate_action_cash",
		"reward_ledger_reconciliation",
		"stock_prices_adjusted",
		"stock_price_history_adjusted",
		"instrument_lineage",
		"reward_quantity_movements",
		"reward_current_balances",
		"reward_ledger_reconciliation",
	} {
		if !relationExists(t, db, view) {
			t.Errorf("required relation %s is missing", view)
		}
	}

	for _, function := range []string{
		"market_today",
		"normalize_instrument_symbol",
		"stock_action_factor",
		"stock_quantity_factor",
		"instrument_terminal_symbol",
		"instrument_delist_event",
		"reward_action_settlement",
		"process_corporate_action_cash",
		"historical_rewards_as_of",
	} {
		if !functionExists(t, db, function) {
			t.Errorf("required function %s is missing", function)
		}
	}

	for _, index := range []string{
		"unique_user_stock_date_normalized",
		"unique_stock_price_symbol_normalized",
		"unique_stock_price_history_symbol_date_normalized",
		"idx_rewards_user_date",
		"idx_ledger_reward_id",
		"idx_adjustments_reward_id",
	} {
		if !indexExists(t, db, index) {
			t.Errorf("required index %s is missing", index)
		}
	}

	// A second Up() must report no change rather than re-running migrations.
	applyMigrations(t, dbURL, -1)

	var stillDirty bool
	if err := db.QueryRow(`SELECT dirty FROM schema_migrations`).Scan(&stillDirty); err != nil {
		t.Fatalf("read dirty flag after second migration: %v", err)
	}
	if stillDirty {
		t.Error("schema_migrations became dirty after re-running migrations")
	}
}

// TestTransactionalWritesIntegration exercises the HTTP write paths against a
// real PostgreSQL database: ledger creation, duplicate protection, reward
// replay after an adjustment, adjustment idempotency, zero-balance policy, and
// reconciliation.
func TestTransactionalWritesIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	router := integrationRouter(db)

	userID := insertIntegrationUser(t, db, "transactional-main")
	seedStockPrice(t, db, "TXMAIN", 100)

	createPayload := map[string]any{
		"user_id":         userID,
		"stock_symbol":    " txmain ",
		"quantity":        10,
		"idempotency_key": "txn-reward-key",
	}
	created := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", createPayload)
	requireStatus(t, created, http.StatusOK)
	rewardID := bodyInt(t, created.body, "rewardId")
	originalRewardResponse := append([]byte(nil), created.raw...)

	if got := scalarString(t, db, `SELECT stock_symbol FROM rewards WHERE id = $1`, rewardID); got != "TXMAIN" {
		t.Errorf("stored reward symbol = %q, want TXMAIN", got)
	}
	if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, rewardID); got != 10 {
		t.Errorf("stored reward quantity = %v, want 10", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 5 {
		t.Errorf("reward ledger row count = %d, want 5", got)
	}
	if !scalarBool(t, db, `SELECT is_reconciled FROM reward_ledger_reconciliation WHERE reward_id = $1`, rewardID) {
		t.Error("fresh reward is not reconciled")
	}

	t.Run("duplicate without an idempotency key is rejected", func(t *testing.T) {
		duplicate := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", map[string]any{
			"user_id":      userID,
			"stock_symbol": "TXMAIN",
			"quantity":     10,
		})
		requireStatus(t, duplicate, http.StatusBadRequest)

		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE user_id = $1`, userID); got != 1 {
			t.Errorf("reward count after duplicate = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 5 {
			t.Errorf("ledger count after duplicate = %d, want 5", got)
		}
	})

	t.Run("same key with a different payload conflicts", func(t *testing.T) {
		conflict := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", map[string]any{
			"user_id":         userID,
			"stock_symbol":    "TXMAIN",
			"quantity":        11,
			"idempotency_key": "txn-reward-key",
		})
		requireStatus(t, conflict, http.StatusConflict)

		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE user_id = $1`, userID); got != 1 {
			t.Errorf("reward count after key conflict = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 5 {
			t.Errorf("ledger count after key conflict = %d, want 5", got)
		}
	})

	adjustmentPayload := map[string]any{
		"adjustment_type": "reward_reversal",
		"delta_quantity":  -2,
		"delta_amount":    200,
		"reason":          "replay proof",
		"idempotency_key": "txn-adjustment-key",
	}
	adjusted := doIntegrationRequest(t, router, http.MethodPost, fmt.Sprintf("/api/v1/adjustments/%d", rewardID), adjustmentPayload)
	requireStatus(t, adjusted, http.StatusOK)
	adjustmentID := nestedBodyInt(t, adjusted.body, "data", "id")
	originalAdjustmentResponse := append([]byte(nil), adjusted.raw...)

	if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, rewardID); got != 8 {
		t.Errorf("reward quantity after reversal = %v, want 8", got)
	}

	t.Run("reward retry after an adjustment replays the original response", func(t *testing.T) {
		replay := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", createPayload)
		requireStatus(t, replay, http.StatusOK)

		if got := bodyInt(t, replay.body, "rewardId"); got != rewardID {
			t.Errorf("replay rewardId = %d, want %d", got, rewardID)
		}
		if !jsonBytesEqual(replay.raw, originalRewardResponse) {
			t.Errorf("replay response differs from the original response:\nreplay: %s\noriginal: %s", replay.raw, originalRewardResponse)
		}
		if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, rewardID); got != 8 {
			t.Errorf("reward quantity after replay = %v, want adjusted value 8", got)
		}
	})

	t.Run("adjustment retry replays and a changed payload conflicts", func(t *testing.T) {
		replay := doIntegrationRequest(t, router, http.MethodPost, fmt.Sprintf("/api/v1/adjustments/%d", rewardID), adjustmentPayload)
		requireStatus(t, replay, http.StatusOK)
		if got := nestedBodyInt(t, replay.body, "data", "id"); got != adjustmentID {
			t.Errorf("adjustment replay id = %d, want %d", got, adjustmentID)
		}
		if !jsonBytesEqual(replay.raw, originalAdjustmentResponse) {
			t.Errorf("adjustment replay differs:\nreplay: %s\noriginal: %s", replay.raw, originalAdjustmentResponse)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM adjustments WHERE idempotency_key = $1`, "txn-adjustment-key"); got != 1 {
			t.Errorf("adjustment row count after retry = %d, want 1", got)
		}

		conflict := doIntegrationRequest(t, router, http.MethodPost, fmt.Sprintf("/api/v1/adjustments/%d", rewardID), map[string]any{
			"adjustment_type": "reward_reversal",
			"delta_quantity":  -1,
			"delta_amount":    100,
			"reason":          "different payload",
			"idempotency_key": "txn-adjustment-key",
		})
		requireStatus(t, conflict, http.StatusConflict)
		if got := countRows(t, db, `SELECT COUNT(*) FROM adjustments WHERE idempotency_key = $1`, "txn-adjustment-key"); got != 1 {
			t.Errorf("adjustment row count after conflict = %d, want 1", got)
		}
		if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, rewardID); got != 8 {
			t.Errorf("reward quantity after adjustment conflict = %v, want 8", got)
		}
		if !scalarBool(t, db, `SELECT is_reconciled FROM reward_ledger_reconciliation WHERE reward_id = $1`, rewardID) {
			t.Error("reward is not reconciled after adjustment")
		}
	})

	t.Run("full reversal closes the position but keeps the audit trail", func(t *testing.T) {
		seedStockPrice(t, db, "TXZERO", 100)
		created := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", map[string]any{
			"user_id":         userID,
			"stock_symbol":    "TXZERO",
			"quantity":        3,
			"idempotency_key": "txn-zero-reward-key",
		})
		requireStatus(t, created, http.StatusOK)
		zeroRewardID := bodyInt(t, created.body, "rewardId")

		reversal := doIntegrationRequest(t, router, http.MethodPost, fmt.Sprintf("/api/v1/adjustments/%d", zeroRewardID), map[string]any{
			"adjustment_type": "reward_reversal",
			"delta_quantity":  -3,
			"delta_amount":    300,
			"reason":          "full reversal",
			"idempotency_key": "txn-zero-adjustment-key",
		})
		requireStatus(t, reversal, http.StatusOK)

		if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, zeroRewardID); got != 0 {
			t.Errorf("fully reversed reward quantity = %v, want 0", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM today_rewards WHERE reward_event_id = $1`, zeroRewardID); got != 0 {
			t.Errorf("zero balance appears in today_rewards: count = %d", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM user_portfolio WHERE user_id = $1 AND stock_symbol = 'TXZERO'`, userID); got != 0 {
			t.Errorf("zero balance appears in user_portfolio: count = %d", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, zeroRewardID); got != 7 {
			t.Errorf("fully reversed reward ledger count = %d, want 7", got)
		}
		if !scalarBool(t, db, `SELECT is_reconciled FROM reward_ledger_reconciliation WHERE reward_id = $1`, zeroRewardID) {
			t.Error("fully reversed reward is not reconciled")
		}
	})

	t.Run("reconciliation detects a malformed stock posting", func(t *testing.T) {
		seedStockPrice(t, db, "TXCORRUPT", 100)
		created := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", map[string]any{
			"user_id":         userID,
			"stock_symbol":    "TXCORRUPT",
			"quantity":        2,
			"idempotency_key": "txn-corrupt-reward-key",
		})
		requireStatus(t, created, http.StatusOK)
		corruptRewardID := bodyInt(t, created.body, "rewardId")

		if !scalarBool(t, db, `SELECT is_reconciled FROM reward_ledger_reconciliation WHERE reward_id = $1`, corruptRewardID) {
			t.Fatal("fresh corrupt-test reward is not reconciled")
		}

		mustExec(t, db, `
			INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount)
			VALUES ($1, 'stock_units', 'TXCORRUPT', 1, 0)
		`, corruptRewardID)

		if scalarBool(t, db, `SELECT is_reconciled FROM reward_ledger_reconciliation WHERE reward_id = $1`, corruptRewardID) {
			t.Error("reconciliation did not detect an extra stock_units posting")
		}
	})
}

// TestConcurrentWritesIntegration proves that concurrent HTTP retries and
// concurrent quantity adjustments produce one committed effect per logical
// operation.
func TestConcurrentWritesIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	router := integrationRouter(db)

	t.Run("concurrent identical rewards with one key commit once", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "concurrent-reward-key")
		seedStockPrice(t, db, "CONCURKEY", 100)

		payload := map[string]any{
			"user_id":         userID,
			"stock_symbol":    "CONCURKEY",
			"quantity":        10,
			"idempotency_key": "concurrent-reward-key",
		}
		responses := runConcurrentRequests(router, http.MethodPost, "/api/v1/reward", repeatedPayload(payload, 8))
		rewardID := -1
		for i, response := range responses {
			if response.status != http.StatusOK {
				t.Fatalf("request %d status = %d, want 200; body: %s", i, response.status, response.raw)
			}
			id := bodyInt(t, response.body, "rewardId")
			if rewardID == -1 {
				rewardID = id
			} else if id != rewardID {
				t.Errorf("request %d returned rewardId %d, want %d", i, id, rewardID)
			}
		}

		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE user_id = $1 AND stock_symbol = 'CONCURKEY'`, userID); got != 1 {
			t.Errorf("concurrent keyed reward count = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 5 {
			t.Errorf("concurrent keyed ledger count = %d, want 5", got)
		}
	})

	t.Run("concurrent same-day rewards without keys have one winner", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "concurrent-reward-nokey")
		seedStockPrice(t, db, "CONCURNOKEY", 100)

		payload := map[string]any{
			"user_id":      userID,
			"stock_symbol": "CONCURNOKEY",
			"quantity":     10,
		}
		responses := runConcurrentRequests(router, http.MethodPost, "/api/v1/reward", repeatedPayload(payload, 8))
		successes, conflicts := 0, 0
		for i, response := range responses {
			switch response.status {
			case http.StatusOK:
				successes++
			case http.StatusBadRequest:
				conflicts++
			default:
				t.Errorf("request %d status = %d, want 200 or 400; body: %s", i, response.status, response.raw)
			}
		}
		if successes != 1 {
			t.Errorf("concurrent unkeyed reward successes = %d, want 1", successes)
		}
		if conflicts != 7 {
			t.Errorf("concurrent unkeyed reward conflicts = %d, want 7", conflicts)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE user_id = $1`, userID); got != 1 {
			t.Errorf("concurrent unkeyed reward count = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id IN (SELECT id FROM rewards WHERE user_id = $1)`, userID); got != 5 {
			t.Errorf("concurrent unkeyed ledger count = %d, want 5", got)
		}
	})

	t.Run("concurrent identical adjustments replay one effect", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "concurrent-adjustment-key")
		seedStockPrice(t, db, "CONCURADJ", 100)
		rewardID := createRewardThroughAPI(t, router, userID, "CONCURADJ", 10, "")

		payload := map[string]any{
			"adjustment_type": "manual_correction",
			"delta_quantity":  -1,
			"delta_amount":    0,
			"reason":          "concurrent retry",
			"idempotency_key": "concurrent-adjustment-key",
		}
		responses := runConcurrentRequests(router, http.MethodPost, fmt.Sprintf("/api/v1/adjustments/%d", rewardID), repeatedPayload(payload, 8))
		adjustmentID := -1
		for i, response := range responses {
			if response.status != http.StatusOK {
				t.Fatalf("adjustment request %d status = %d, want 200; body: %s", i, response.status, response.raw)
			}
			id := nestedBodyInt(t, response.body, "data", "id")
			if adjustmentID == -1 {
				adjustmentID = id
			} else if id != adjustmentID {
				t.Errorf("adjustment request %d returned id %d, want %d", i, id, adjustmentID)
			}
		}

		if got := countRows(t, db, `SELECT COUNT(*) FROM adjustments WHERE idempotency_key = $1`, "concurrent-adjustment-key"); got != 1 {
			t.Errorf("concurrent keyed adjustment count = %d, want 1", got)
		}
		if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, rewardID); got != 9 {
			t.Errorf("reward quantity after concurrent adjustment retries = %v, want 9", got)
		}
	})

	t.Run("concurrent overdrafts apply only the affordable adjustments", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "concurrent-adjustment-overdraft")
		seedStockPrice(t, db, "CONCUROVER", 100)
		rewardID := createRewardThroughAPI(t, router, userID, "CONCUROVER", 5, "")

		payloads := make([]any, 4)
		for i := range payloads {
			payloads[i] = map[string]any{
				"adjustment_type": "manual_correction",
				"delta_quantity":  -2,
				"delta_amount":    0,
				"reason":          "concurrent overdraft",
				"idempotency_key": fmt.Sprintf("concurrent-overdraft-%d", i),
			}
		}
		responses := runConcurrentRequests(router, http.MethodPost, fmt.Sprintf("/api/v1/adjustments/%d", rewardID), payloads)
		successes, rejections := 0, 0
		for i, response := range responses {
			switch response.status {
			case http.StatusOK:
				successes++
			case http.StatusBadRequest:
				rejections++
			default:
				t.Errorf("adjustment request %d status = %d, want 200 or 400; body: %s", i, response.status, response.raw)
			}
		}
		if successes != 2 {
			t.Errorf("affordable concurrent adjustments = %d, want 2", successes)
		}
		if rejections != 2 {
			t.Errorf("rejected concurrent overdrafts = %d, want 2", rejections)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM adjustments WHERE reward_id = $1`, rewardID); got != 2 {
			t.Errorf("overdraft adjustment count = %d, want 2", got)
		}
		if got := scalarFloat(t, db, `SELECT quantity::float8 FROM rewards WHERE id = $1`, rewardID); got != 1 {
			t.Errorf("reward quantity after concurrent overdrafts = %v, want 1", got)
		}
		if !scalarBool(t, db, `SELECT is_reconciled FROM reward_ledger_reconciliation WHERE reward_id = $1`, rewardID) {
			t.Error("reward is not reconciled after concurrent adjustments")
		}
	})
}

// TestValuationViewsIntegration verifies the shared valuation views against
// real corporate-action rows: splits, bonuses, reverse splits, a merger chain
// with a successor split, terminal delists, and cash-in-lieu settlement.
func TestValuationViewsIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	router := integrationRouter(db)

	t.Run("split adjusts quantity and price without changing value", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "view-split")
		rewardID := insertHistoricalReward(t, db, userID, "VIEWSPLIT", 10, 10)
		seedStockPrice(t, db, "VIEWSPLIT", 100)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES ('VIEWSPLIT', 'split', 2, 1, market_today() - 5)
		`)

		var quantity, price, value float64
		var status string
		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, current_price::float8, inr_value::float8, price_status
			FROM user_portfolio
			WHERE user_id = $1 AND stock_symbol = 'VIEWSPLIT'
		`, userID).Scan(&quantity, &price, &value, &status); err != nil {
			t.Fatalf("read split portfolio valuation: %v", err)
		}
		if quantity != 20 || price != 50 || value != 1000 || status != "available" {
			t.Errorf("split portfolio valuation = qty %v price %v value %v status %q, want 20/50/1000/available", quantity, price, value, status)
		}

		var active bool
		var terminal string
		if err := db.QueryRow(`
			SELECT is_active, terminal_symbol
			FROM reward_corporate_action_outcomes
			WHERE reward_id = $1
		`, rewardID).Scan(&active, &terminal); err != nil {
			t.Fatalf("read split outcome: %v", err)
		}
		if !active || terminal != "VIEWSPLIT" {
			t.Errorf("split outcome = active %v terminal %q, want true/VIEWSPLIT", active, terminal)
		}
	})

	t.Run("grant-date history is not restated by a later split", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "view-history")
		rewardID := insertHistoricalReward(t, db, userID, "VIEWHIST", 10, 5)
		rewardDate := scalarString(t, db, `SELECT reward_date::text FROM rewards WHERE id = $1`, rewardID)

		mustExec(t, db, `
			INSERT INTO stock_price_history (stock_symbol, price, date)
			VALUES ('VIEWHIST', 200, $1::date), ('VIEWHIST', 200, market_today())
		`, rewardDate)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES ('VIEWHIST', 'split', 2, 1, market_today())
		`)

		var quantity, price, value float64
		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, price::float8, inr_value::float8
			FROM historical_rewards
			WHERE reward_event_id = $1
		`, rewardID).Scan(&quantity, &price, &value); err != nil {
			t.Fatalf("read grant-date valuation: %v", err)
		}
		if quantity != 10 || price != 200 || value != 2000 {
			t.Errorf("grant-date valuation = qty %v price %v value %v, want 10/200/2000", quantity, price, value)
		}

		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, price::float8, inr_value::float8
			FROM historical_rewards_as_of(market_today())
			WHERE reward_event_id = $1
		`, rewardID).Scan(&quantity, &price, &value); err != nil {
			t.Fatalf("read split as-of history valuation: %v", err)
		}
		if quantity != 20 || price != 100 || value != 2000 {
			t.Errorf("split as-of history valuation = qty %v price %v value %v, want 20/100/2000", quantity, price, value)
		}

		response := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/historical-inr/%d", userID), nil)
		requireStatus(t, response, http.StatusOK)
		history := bodyArray(t, response.body, "history")
		if len(history) != 1 {
			t.Fatalf("historical response row count = %d, want 1", len(history))
		}
		row := history[0].(map[string]any)
		if row["adjustedQuantity"] != float64(10) || row["price"] != float64(200) || row["inrValue"] != float64(2000) {
			t.Errorf("historical API row = %v, want adjustedQuantity 10, price 200, inrValue 2000", row)
		}
	})

	t.Run("bonus and reverse split factors", func(t *testing.T) {
		bonusUserID := insertIntegrationUser(t, db, "view-bonus")
		insertHistoricalReward(t, db, bonusUserID, "VIEWBONUS", 10, 10)
		seedStockPrice(t, db, "VIEWBONUS", 100)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES ('VIEWBONUS', 'bonus', 2, 1, market_today() - 5)
		`)

		var quantity, price, value float64
		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, current_price::float8, inr_value::float8
			FROM user_portfolio
			WHERE user_id = $1 AND stock_symbol = 'VIEWBONUS'
		`, bonusUserID).Scan(&quantity, &price, &value); err != nil {
			t.Fatalf("read bonus valuation: %v", err)
		}
		if quantity != 20 || price != 50 || value != 1000 {
			t.Errorf("bonus valuation = qty %v price %v value %v, want 20/50/1000", quantity, price, value)
		}

		reverseUserID := insertIntegrationUser(t, db, "view-reverse")
		insertHistoricalReward(t, db, reverseUserID, "VIEWREVERSE", 20, 10)
		seedStockPrice(t, db, "VIEWREVERSE", 100)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES ('VIEWREVERSE', 'split', 1, 5, market_today() - 5)
		`)

		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, current_price::float8, inr_value::float8
			FROM user_portfolio
			WHERE user_id = $1 AND stock_symbol = 'VIEWREVERSE'
		`, reverseUserID).Scan(&quantity, &price, &value); err != nil {
			t.Fatalf("read reverse split valuation: %v", err)
		}
		if quantity != 4 || price != 500 || value != 2000 {
			t.Errorf("reverse split valuation = qty %v price %v value %v, want 4/500/2000", quantity, price, value)
		}
	})

	t.Run("merger chain composes ratios, successor events, and predecessor delists", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "view-chain")
		rewardID := insertHistoricalReward(t, db, userID, "VIEWA", 10, 20)
		seedStockPrice(t, db, "VIEWC", 100)

		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date, new_symbol)
			VALUES
				('VIEWA', 'merge', 2, 1, market_today() - 10, 'VIEWB'),
				('VIEWB', 'merge', 3, 1, market_today() - 5, 'VIEWC')
		`)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
			VALUES ('VIEWC', 'split', 2, 1, market_today())
		`)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date, delist_outcome)
			VALUES ('VIEWA', 'delist', 1, 1, market_today() - 3, 'write_off')
		`)

		if got := scalarString(t, db, `SELECT instrument_terminal_symbol('VIEWA', (SELECT reward_date FROM rewards WHERE id = $1), market_today())`, rewardID); got != "VIEWC" {
			t.Errorf("terminal symbol = %q, want VIEWC", got)
		}

		var symbol string
		var quantity, price, value float64
		if err := db.QueryRow(`
			SELECT stock_symbol, adjusted_quantity::float8, current_price::float8, inr_value::float8
			FROM user_portfolio
			WHERE user_id = $1
		`, userID).Scan(&symbol, &quantity, &price, &value); err != nil {
			t.Fatalf("read chain valuation: %v", err)
		}
		if symbol != "VIEWC" || quantity != 120 || price != 50 || value != 6000 {
			t.Errorf("chain valuation = symbol %q qty %v price %v value %v, want VIEWC/120/50/6000", symbol, quantity, price, value)
		}

		if !scalarBool(t, db, `SELECT is_active FROM reward_corporate_action_outcomes WHERE reward_id = $1`, rewardID) {
			t.Error("predecessor delist removed the valid successor holding")
		}
	})

	t.Run("terminal delist outcomes are consistent across portfolio and stats", func(t *testing.T) {
		cashUserID := insertIntegrationUser(t, db, "view-delist-cash")
		insertHistoricalReward(t, db, cashUserID, "VIEWDELCASH", 10, 10)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date, delist_outcome, cash_per_unit)
			VALUES ('VIEWDELCASH', 'delist', 1, 1, market_today(), 'cash_exit', 150)
		`)

		portfolio := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/portfolio/%d", cashUserID), nil)
		requireStatus(t, portfolio, http.StatusOK)
		stats := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/stats/%d", cashUserID), nil)
		requireStatus(t, stats, http.StatusOK)

		for name, body := range map[string]map[string]any{"portfolio": portfolio.body, "stats": stats.body} {
			if got := bodyFloat(t, body, "totalPortfolioValue"); got != 0 {
				t.Errorf("%s totalPortfolioValue after cash exit = %v, want 0", name, got)
			}
			if got := bodyInt(t, body, "totalPositions"); got != 0 {
				t.Errorf("%s totalPositions after cash exit = %d, want 0", name, got)
			}
			if got := bodyFloat(t, body, "cashProceeds"); got != 1500 {
				t.Errorf("%s cashProceeds after cash exit = %v, want 1500", name, got)
			}
		}
		if got := bodyString(t, portfolio.body, "valuationDate"); got != scalarString(t, db, `SELECT market_today()::text`) {
			t.Errorf("portfolio valuationDate = %q, want market_today()", got)
		}
		if got := bodyString(t, stats.body, "valuationDate"); got != bodyString(t, portfolio.body, "valuationDate") {
			t.Errorf("stats valuationDate = %q, want portfolio valuationDate %q", got, bodyString(t, portfolio.body, "valuationDate"))
		}

		writeOffUserID := insertIntegrationUser(t, db, "view-delist-writeoff")
		insertHistoricalReward(t, db, writeOffUserID, "VIEWDELWO", 5, 10)
		mustExec(t, db, `
			INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date, delist_outcome)
			VALUES ('VIEWDELWO', 'delist', 1, 1, market_today(), 'write_off')
		`)

		writeOff := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/portfolio/%d", writeOffUserID), nil)
		requireStatus(t, writeOff, http.StatusOK)
		if got := bodyFloat(t, writeOff.body, "totalPortfolioValue"); got != 0 {
			t.Errorf("write-off totalPortfolioValue = %v, want 0", got)
		}
		if got := bodyFloat(t, writeOff.body, "writtenOffQuantity"); got != 5 {
			t.Errorf("writtenOffQuantity = %v, want 5", got)
		}
		if got := bodyFloat(t, writeOff.body, "cashProceeds"); got != 0 {
			t.Errorf("write-off cashProceeds = %v, want 0", got)
		}
	})

	t.Run("cash-in-lieu settles a fractional reverse split", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "view-cash-in-lieu")
		rewardID := insertHistoricalReward(t, db, userID, "VIEWCIL", 1, 10)
		seedStockPrice(t, db, "VIEWCIL", 100)
		mustExec(t, db, `
			INSERT INTO stock_events (
				stock_symbol, event_type, ratio_num, ratio_den, effective_date,
				fraction_policy, fraction_cash_per_unit
			)
			VALUES ('VIEWCIL', 'split', 1, 5, market_today() - 5, 'cash_in_lieu', 100)
		`)

		var settled, fraction, cash float64
		if err := db.QueryRow(`
			SELECT settled_quantity::float8, fraction_quantity::float8, cash_in_lieu::float8
			FROM reward_action_settlement($1, market_today())
		`, rewardID).Scan(&settled, &fraction, &cash); err != nil {
			t.Fatalf("read cash-in-lieu settlement: %v", err)
		}
		if settled != 0 || fraction != 0.2 || cash != 20 {
			t.Errorf("cash-in-lieu settlement = settled %v fraction %v cash %v, want 0/0.2/20", settled, fraction, cash)
		}

		var quantity, proceeds float64
		if err := db.QueryRow(`
			SELECT adjusted_quantity::float8, cash_in_lieu_proceeds::float8
			FROM reward_corporate_action_outcomes
			WHERE reward_id = $1
		`, rewardID).Scan(&quantity, &proceeds); err != nil {
			t.Fatalf("read cash-in-lieu outcome: %v", err)
		}
		if quantity != 0 || proceeds != 20 {
			t.Errorf("cash-in-lieu outcome = quantity %v proceeds %v, want 0/20", quantity, proceeds)
		}
	})
}

// TestMissingPriceIntegration verifies that missing current and historical
// prices remain visible with explicit status and do not silently reduce totals.
func TestMissingPriceIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)
	router := integrationRouter(db)

	t.Run("today, portfolio, and stats keep an unpriced current holding", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "missing-current")
		insertTodayReward(t, db, userID, "MISSINGCUR", 4)

		today := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/today-stocks/%d", userID), nil)
		requireStatus(t, today, http.StatusOK)
		if got := bodyBool(t, today.body, "valuationComplete"); got {
			t.Error("today valuationComplete = true with a missing current price")
		}
		if got := bodyInt(t, today.body, "unpricedRewards"); got != 1 {
			t.Errorf("today unpricedRewards = %d, want 1", got)
		}
		stock := findJSONObject(t, bodyArray(t, today.body, "stocks"), "stockSymbol", "MISSINGCUR")
		if stock["priceStatus"] != "missing" {
			t.Errorf("today priceStatus = %v, want missing", stock["priceStatus"])
		}
		if stock["currentPrice"] != nil || stock["inrValue"] != nil {
			t.Errorf("today missing-price values = currentPrice %v inrValue %v, want null/null", stock["currentPrice"], stock["inrValue"])
		}

		portfolio := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/portfolio/%d", userID), nil)
		requireStatus(t, portfolio, http.StatusOK)
		if got := bodyBool(t, portfolio.body, "valuationComplete"); got {
			t.Error("portfolio valuationComplete = true with a missing current price")
		}
		if got := bodyFloat(t, portfolio.body, "totalPortfolioValue"); got != 0 {
			t.Errorf("portfolio totalPortfolioValue = %v, want 0", got)
		}
		if got := bodyInt(t, portfolio.body, "totalPositions"); got != 1 {
			t.Errorf("portfolio totalPositions = %d, want 1", got)
		}
		if got := bodyInt(t, portfolio.body, "unpricedPositions"); got != 1 {
			t.Errorf("portfolio unpricedPositions = %d, want 1", got)
		}
		item := findJSONObject(t, bodyArray(t, portfolio.body, "portfolio"), "stockSymbol", "MISSINGCUR")
		if item["priceStatus"] != "missing" || item["currentPrice"] != nil || item["inrValue"] != nil {
			t.Errorf("portfolio missing-price item = %v, want missing with null values", item)
		}

		stats := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/stats/%d", userID), nil)
		requireStatus(t, stats, http.StatusOK)
		if got := bodyBool(t, stats.body, "valuationComplete"); got {
			t.Error("stats valuationComplete = true with a missing current price")
		}
		if got := bodyFloat(t, stats.body, "totalPortfolioValue"); got != 0 {
			t.Errorf("stats totalPortfolioValue = %v, want 0", got)
		}
		if got := bodyInt(t, stats.body, "totalPositions"); got != 1 {
			t.Errorf("stats totalPositions = %d, want 1", got)
		}
		if got := bodyInt(t, stats.body, "unpricedPositions"); got != 1 {
			t.Errorf("stats unpricedPositions = %d, want 1", got)
		}
	})

	t.Run("historical reporting keeps an unpriced past reward", func(t *testing.T) {
		userID := insertIntegrationUser(t, db, "missing-history")
		insertHistoricalReward(t, db, userID, "MISSINGHIST", 3, 5)

		response := doIntegrationRequest(t, router, http.MethodGet, fmt.Sprintf("/api/v1/historical-inr/%d", userID), nil)
		requireStatus(t, response, http.StatusOK)
		if got := bodyBool(t, response.body, "valuationComplete"); got {
			t.Error("historical valuationComplete = true with a missing reward-date price")
		}
		if got := bodyInt(t, response.body, "unpricedRewards"); got != 1 {
			t.Errorf("historical unpricedRewards = %d, want 1", got)
		}
		row := findJSONObject(t, bodyArray(t, response.body, "history"), "stockSymbol", "MISSINGHIST")
		if row["priceStatus"] != "missing" {
			t.Errorf("historical priceStatus = %v, want missing", row["priceStatus"])
		}
		if row["price"] != nil || row["inrValue"] != nil {
			t.Errorf("historical missing-price values = price %v inrValue %v, want null/null", row["price"], row["inrValue"])
		}
	})
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type integrationResponse struct {
	status int
	body   map[string]any
	raw    []byte
}

func integrationRouter(db *sql.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	stocky.Routes(router, stocky.NewHandler(db))
	return router
}

func doIntegrationRequest(t *testing.T, router http.Handler, method, path string, payload any) integrationResponse {
	t.Helper()

	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode request payload: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request := httptest.NewRequest(method, path, reader)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	response := integrationResponse{
		status: recorder.Code,
		raw:    append([]byte(nil), recorder.Body.Bytes()...),
	}
	if len(response.raw) > 0 {
		if err := json.Unmarshal(response.raw, &response.body); err != nil {
			t.Fatalf("decode response body %q: %v", response.raw, err)
		}
	}
	return response
}

func performIntegrationRequest(router http.Handler, method, path string, payload any) integrationResponse {
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		reader = bytes.NewReader(encoded)
	}

	request := httptest.NewRequest(method, path, reader)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	response := integrationResponse{
		status: recorder.Code,
		raw:    append([]byte(nil), recorder.Body.Bytes()...),
	}
	if len(response.raw) > 0 {
		_ = json.Unmarshal(response.raw, &response.body)
	}
	return response
}

func runConcurrentRequests(router http.Handler, method, path string, payloads []any) []integrationResponse {
	responses := make([]integrationResponse, len(payloads))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range payloads {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			responses[i] = performIntegrationRequest(router, method, path, payloads[i])
		}(i)
	}
	close(start)
	wg.Wait()
	return responses
}

func repeatedPayload(payload any, count int) []any {
	payloads := make([]any, count)
	for i := range payloads {
		payloads[i] = payload
	}
	return payloads
}

func createRewardThroughAPI(t *testing.T, router http.Handler, userID int, symbol string, quantity float64, key string) int {
	t.Helper()

	payload := map[string]any{
		"user_id":      userID,
		"stock_symbol": symbol,
		"quantity":     quantity,
	}
	if key != "" {
		payload["idempotency_key"] = key
	}
	response := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", payload)
	requireStatus(t, response, http.StatusOK)
	return bodyInt(t, response.body, "rewardId")
}

func seedStockPrice(t *testing.T, db *sql.DB, symbol string, price float64) {
	t.Helper()
	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (stock_symbol) DO UPDATE
		SET price = EXCLUDED.price, updated_at = EXCLUDED.updated_at
	`, symbol, price)
}

func insertTodayReward(t *testing.T, db *sql.DB, userID int, symbol string, quantity float64) int {
	t.Helper()

	var id int
	if err := db.QueryRow(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity)
		VALUES ($1, $2, $3, $3)
		RETURNING id
	`, userID, symbol, quantity).Scan(&id); err != nil {
		t.Fatalf("insert today reward %s: %v", symbol, err)
	}
	return id
}

func insertHistoricalReward(t *testing.T, db *sql.DB, userID int, symbol string, quantity float64, daysAgo int) int {
	t.Helper()

	var id int
	if err := db.QueryRow(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
		VALUES (
			$1,
			$2,
			$3,
			$3,
			(market_today() - $4::int + TIME '12:00') AT TIME ZONE 'Asia/Kolkata'
		)
		RETURNING id
	`, userID, symbol, quantity, daysAgo).Scan(&id); err != nil {
		t.Fatalf("insert historical reward %s: %v", symbol, err)
	}
	return id
}

func requireStatus(t *testing.T, response integrationResponse, want int) {
	t.Helper()
	if response.status != want {
		t.Fatalf("status = %d, want %d; body: %s", response.status, want, response.raw)
	}
}

func bodyInt(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("response body has no %q: %v", key, body)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("response field %q = %v (%T), want number", key, value, value)
	}
	return int(number)
}

func nestedBodyInt(t *testing.T, body map[string]any, parent, key string) int {
	t.Helper()
	nested, ok := body[parent].(map[string]any)
	if !ok {
		t.Fatalf("response field %q = %v (%T), want object", parent, body[parent], body[parent])
	}
	return bodyInt(t, nested, key)
}

func bodyFloat(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("response body has no %q: %v", key, body)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("response field %q = %v (%T), want number", key, value, value)
	}
	return number
}

func bodyBool(t *testing.T, body map[string]any, key string) bool {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("response body has no %q: %v", key, body)
	}
	boolean, ok := value.(bool)
	if !ok {
		t.Fatalf("response field %q = %v (%T), want bool", key, value, value)
	}
	return boolean
}

func bodyString(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("response body has no %q: %v", key, body)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("response field %q = %v (%T), want string", key, value, value)
	}
	return text
}

func bodyArray(t *testing.T, body map[string]any, key string) []any {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("response body has no %q: %v", key, body)
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("response field %q = %v (%T), want array", key, value, value)
	}
	return items
}

func findJSONObject(t *testing.T, items []any, key, value string) map[string]any {
	t.Helper()
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if object[key] == value {
			return object
		}
	}
	t.Fatalf("no object with %s = %q in %v", key, value, items)
	return nil
}

func jsonBytesEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return bytes.Equal(left, right)
	}
	leftJSON, err := json.Marshal(leftValue)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(rightValue)
	if err != nil {
		return false
	}
	return bytes.Equal(leftJSON, rightJSON)
}

func relationExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var relation sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.' || $1)::text`, name).Scan(&relation); err != nil {
		t.Fatalf("check relation %s: %v", name, err)
	}
	return relation.Valid
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()

	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v\nquery: %s", err, query)
	}
	return count
}

func scalarFloat(t *testing.T, db *sql.DB, query string, args ...any) float64 {
	t.Helper()

	var value float64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read float: %v\nquery: %s", err, query)
	}
	return value
}

func scalarString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()

	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read string: %v\nquery: %s", err, query)
	}
	return value
}

func scalarBool(t *testing.T, db *sql.DB, query string, args ...any) bool {
	t.Helper()

	var value bool
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read bool: %v\nquery: %s", err, query)
	}
	return value
}
