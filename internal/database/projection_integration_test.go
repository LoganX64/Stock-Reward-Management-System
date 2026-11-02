package database

import (
	"testing"
)

// TestReadProjectionsIntegration exercises the Phase 1 projection foundation:
// direct-SQL triggers, compatibility-view parity, skip mode, rollover refresh,
// and the factor cache.
func TestReadProjectionsIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	userID := insertIntegrationUser(t, db, "projection_user")

	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES
			('ALPHA', 100.0000, 'simulator', NOW()),
			('BETA', 200.0000, 'simulator', NOW()),
			('MERGE', 50.0000, 'simulator', NOW()),
			('SUCC', 25.0000, 'simulator', NOW()),
			('DELIST', 10.0000, 'simulator', NOW()),
			('WRITE', 20.0000, 'simulator', NOW())
	`)

	// created_at drives reward_date through trg_set_reward_date.
	insertReward := func(symbol string, quantity int, daysAgo int) int {
		t.Helper()
		var id int
		if err := db.QueryRow(`
			INSERT INTO rewards (
				user_id, stock_symbol, original_quantity, quantity, created_at
			)
			VALUES (
				$1, $2, $3, $3,
				((market_today() - $4::INTEGER) + time '09:15') AT TIME ZONE 'Asia/Kolkata'
			)
			RETURNING id
		`, userID, symbol, quantity, daysAgo).Scan(&id); err != nil {
			t.Fatalf("insert reward %s: %v", symbol, err)
		}
		return id
	}

	alphaID := insertReward("ALPHA", 10, 30)
	betaID := insertReward("BETA", 20, 30)
	mergeID := insertReward("MERGE", 5, 30)
	delistID := insertReward("DELIST", 7, 30)
	writeID := insertReward("WRITE", 9, 30)
	todayID := insertReward("ALPHA", 3, 0)

	if got := countRows(t, db, `SELECT COUNT(*) FROM reward_state WHERE user_id = $1`, userID); got != 6 {
		t.Fatalf("reward_state rows after direct inserts = %d, want 6", got)
	}

	// Adjustments refresh the reward and the user's aggregates in-transaction.
	mustExec(t, db, `
		INSERT INTO adjustments (
			reward_id, adjustment_type, delta_quantity, delta_amount, reason, effective_date
		)
		VALUES ($1, 'manual_correction', -2, 5, 'projection test', market_today() - 10)
	`, alphaID)

	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, alphaID); got != "8.000000" {
		t.Errorf("alpha adjusted_quantity after adjustment = %s, want 8.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT total_adjustment_amount::TEXT FROM reward_state WHERE reward_id = $1
	`, alphaID); got != "5.0000" {
		t.Errorf("alpha total_adjustment_amount = %s, want 5.0000", got)
	}

	// Split and bonus events refresh the factor and both ALPHA rewards.
	mustExec(t, db, `
		INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
		VALUES ('ALPHA', 'split', 2, 1, market_today() - 20)
	`)
	mustExec(t, db, `
		INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
		VALUES ('BETA', 'bonus', 2, 1, market_today() - 20)
	`)

	if got := scalarString(t, db, `
		SELECT factor::TEXT FROM instrument_factor WHERE stock_symbol = 'ALPHA'
	`); got != "2.0000000000000000" {
		t.Errorf("alpha factor = %s, want 2.0000000000000000", got)
	}
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, alphaID); got != "18.000000" {
		t.Errorf("alpha adjusted_quantity after split = %s, want 18.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, betaID); got != "40.000000" {
		t.Errorf("beta adjusted_quantity after bonus = %s, want 40.000000", got)
	}

	// The current-day ALPHA reward is after the split date and must stay at 3.
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, todayID); got != "3.000000" {
		t.Errorf("today alpha adjusted_quantity = %s, want 3.000000", got)
	}

	// A merger changes the terminal symbol but not stock_action_factor().
	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, new_symbol, effective_date
		)
		VALUES ('MERGE', 'merge', 2, 1, 'SUCC', market_today() - 20)
	`)
	if got := scalarString(t, db, `
		SELECT terminal_symbol FROM reward_state WHERE reward_id = $1
	`, mergeID); got != "SUCC" {
		t.Errorf("merge terminal_symbol = %s, want SUCC", got)
	}
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, mergeID); got != "10.000000" {
		t.Errorf("merge adjusted_quantity = %s, want 10.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT COALESCE(
			(SELECT factor::TEXT FROM instrument_factor WHERE stock_symbol = 'MERGE'),
			'1'
		)
	`); got != "1" {
		t.Errorf("merge factor = %s, want 1 (merges must not change the price factor)", got)
	}

	// Cash exit and write-off.
	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, effective_date,
			delist_outcome, cash_per_unit
		)
		VALUES ('DELIST', 'delist', 1, 1, market_today() - 20, 'cash_exit', 100)
	`)
	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, effective_date,
			delist_outcome
		)
		VALUES ('WRITE', 'delist', 1, 1, market_today() - 20, 'write_off')
	`)

	if got := scalarString(t, db, `
		SELECT cash_proceeds::TEXT FROM reward_state WHERE reward_id = $1
	`, delistID); got != "700.0000" {
		t.Errorf("delist cash_proceeds = %s, want 700.0000", got)
	}
	if got := scalarBool(t, db, `
		SELECT is_active FROM reward_state WHERE reward_id = $1
	`, delistID); got {
		t.Error("delisted reward is still active")
	}
	if got := scalarString(t, db, `
		SELECT written_off_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, writeID); got != "9.000000" {
		t.Errorf("write-off quantity = %s, want 9.000000", got)
	}

	// Compatibility views must match the private base view exactly.
	if got := countRows(t, db, `
		SELECT COUNT(*) FROM (
			(SELECT * FROM reward_corporate_action_outcomes
			 EXCEPT
			 SELECT * FROM reward_corporate_action_outcomes_base)
			UNION ALL
			(SELECT * FROM reward_corporate_action_outcomes_base
			 EXCEPT
			 SELECT * FROM reward_corporate_action_outcomes)
		) mismatches
	`); got != 0 {
		t.Errorf("reward_corporate_action_outcomes drift rows = %d, want 0", got)
	}

	if got := countRows(t, db, `
		SELECT COUNT(*) FROM (
			(SELECT user_id, terminal_symbol,
			        SUM(adjusted_quantity)::NUMERIC AS quantity,
			        SUM(total_adjustment_amount)::NUMERIC AS total
			 FROM reward_corporate_action_outcomes_base
			 WHERE is_active
			 GROUP BY user_id, terminal_symbol
			 EXCEPT
			 SELECT user_id, terminal_symbol, quantity, total_adjustment_amount
			 FROM portfolio_position)
			UNION ALL
			(SELECT user_id, terminal_symbol, quantity, total_adjustment_amount
			 FROM portfolio_position
			 EXCEPT
			 SELECT user_id, terminal_symbol,
			        SUM(adjusted_quantity)::NUMERIC,
			        SUM(total_adjustment_amount)::NUMERIC
			 FROM reward_corporate_action_outcomes_base
			 WHERE is_active
			 GROUP BY user_id, terminal_symbol)
		) mismatches
	`); got != 0 {
		t.Errorf("portfolio_position drift rows = %d, want 0", got)
	}

	if got := countRows(t, db, `
		SELECT COUNT(*) FROM (
			(SELECT user_id,
			        ROUND(COALESCE(SUM(cash_proceeds), 0), 4)::NUMERIC,
			        ROUND(COALESCE(SUM(written_off_quantity), 0), 6)::NUMERIC,
			        ROUND(COALESCE(SUM(cash_in_lieu_proceeds), 0), 4)::NUMERIC
			 FROM reward_corporate_action_outcomes_base
			 GROUP BY user_id
			 EXCEPT
			 SELECT user_id, cash_proceeds, written_off_quantity, cash_in_lieu_proceeds
			 FROM user_corporate_action_cash)
			UNION ALL
			(SELECT user_id, cash_proceeds, written_off_quantity, cash_in_lieu_proceeds
			 FROM user_corporate_action_cash
			 EXCEPT
			 SELECT user_id,
			        ROUND(COALESCE(SUM(cash_proceeds), 0), 4)::NUMERIC,
			        ROUND(COALESCE(SUM(written_off_quantity), 0), 6)::NUMERIC,
			        ROUND(COALESCE(SUM(cash_in_lieu_proceeds), 0), 4)::NUMERIC
			 FROM reward_corporate_action_outcomes_base
			 GROUP BY user_id)
		) mismatches
	`); got != 0 {
		t.Errorf("user_corporate_action_cash drift rows = %d, want 0", got)
	}

	// today_rewards is projection-backed after 0027 and must contain the
	// reward granted today.
	if got := countRows(t, db, `
		SELECT COUNT(*) FROM today_rewards
		WHERE user_id = $1 AND reward_event_id = $2
	`, userID, todayID); got != 1 {
		t.Errorf("today_rewards rows for today's reward = %d, want 1", got)
	}

	// The cache drives stock_prices_adjusted, with a fallback when stale.
	mustExec(t, db, `
		UPDATE instrument_factor SET factor = 4, as_of = market_today()
		WHERE stock_symbol = 'ALPHA'
	`)
	if got := scalarString(t, db, `
		SELECT adjusted_price::TEXT FROM stock_prices_adjusted
		WHERE stock_symbol = 'ALPHA'
	`); got != "25.0000000000000000" {
		t.Errorf("cached adjusted_price = %s, want 25.0000000000000000", got)
	}

	mustExec(t, db, `
		UPDATE instrument_factor SET as_of = market_today() - 1
		WHERE stock_symbol = 'ALPHA'
	`)
	if got := scalarString(t, db, `
		SELECT adjusted_price::TEXT FROM stock_prices_adjusted
		WHERE stock_symbol = 'ALPHA'
	`); got != "50.0000000000000000" {
		t.Errorf("stale-cache adjusted_price = %s, want 50.0000000000000000", got)
	}

	mustExec(t, db, `SELECT refresh_instrument_factors(market_today())`)
	if got := scalarString(t, db, `
		SELECT factor::TEXT FROM instrument_factor WHERE stock_symbol = 'ALPHA'
	`); got != "2.0000000000000000" {
		t.Errorf("refreshed alpha factor = %s, want 2.0000000000000000", got)
	}
	if got := scalarString(t, db, `
		SELECT as_of::TEXT FROM instrument_factor WHERE stock_symbol = 'ALPHA'
	`); got != scalarString(t, db, `SELECT market_today()::TEXT`) {
		t.Errorf("refreshed alpha as_of = %s, want market_today()", got)
	}

	// skip_projection_refresh suppresses trigger work, then an explicit refresh
	// produces exactly the base-view result.
	skipUserID := insertIntegrationUser(t, db, "projection_skip_user")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin skip transaction: %v", err)
	}
	if _, err := tx.Exec(`SET LOCAL stocky.skip_projection_refresh = 'on'`); err != nil {
		t.Fatalf("set skip GUC: %v", err)
	}
	var skipRewardID int
	if err := tx.QueryRow(`
		INSERT INTO rewards (
			user_id, stock_symbol, original_quantity, quantity, created_at
		)
		VALUES (
			$1, 'BETA', 4, 4,
			((market_today() - 5) + time '09:15') AT TIME ZONE 'Asia/Kolkata'
		)
		RETURNING id
	`, skipUserID).Scan(&skipRewardID); err != nil {
		t.Fatalf("insert skipped reward: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit skip transaction: %v", err)
	}

	if got := countRows(t, db, `
		SELECT COUNT(*) FROM reward_state WHERE reward_id = $1
	`, skipRewardID); got != 0 {
		t.Fatalf("skip_projection_refresh created %d projection rows, want 0", got)
	}

	mustExec(t, db, `SELECT refresh_reward_state($1)`, skipRewardID)
	mustExec(t, db, `SELECT refresh_user_projections($1)`, skipUserID)

	if got := countRows(t, db, `
		SELECT COUNT(*) FROM (
			SELECT reward_id, adjusted_quantity, total_adjustment_amount, is_active
			FROM reward_state WHERE reward_id = $1
			EXCEPT
			SELECT reward_id, adjusted_quantity, total_adjustment_amount, is_active
			FROM reward_corporate_action_outcomes_base WHERE reward_id = $1
		) mismatches
	`, skipRewardID); got != 0 {
		t.Errorf("explicit refresh drift rows = %d, want 0", got)
	}

	// Rollover: a reward inserted with skip mode and a future-dated adjustment
	// are caught up by refresh_effective_corporate_actions.
	rolloverUserID := insertIntegrationUser(t, db, "projection_rollover_user")
	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("begin rollover transaction: %v", err)
	}
	if _, err := tx.Exec(`SET LOCAL stocky.skip_projection_refresh = 'on'`); err != nil {
		t.Fatalf("set rollover skip GUC: %v", err)
	}
	var rolloverRewardID int
	if err := tx.QueryRow(`
		INSERT INTO rewards (
			user_id, stock_symbol, original_quantity, quantity, created_at
		)
		VALUES (
			$1, 'BETA', 6, 6,
			((market_today() - 5) + time '09:15') AT TIME ZONE 'Asia/Kolkata'
		)
		RETURNING id
	`, rolloverUserID).Scan(&rolloverRewardID); err != nil {
		t.Fatalf("insert rollover reward: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO adjustments (
			reward_id, adjustment_type, delta_quantity, delta_amount, reason, effective_date
		)
		VALUES ($1, 'manual_correction', -1, 2, 'future adjustment', market_today())
	`, rolloverRewardID); err != nil {
		t.Fatalf("insert rollover adjustment: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rollover transaction: %v", err)
	}

	if got := countRows(t, db, `
		SELECT COUNT(*) FROM reward_state WHERE reward_id = $1
	`, rolloverRewardID); got != 0 {
		t.Fatalf("rollover reward has %d projection rows before rollover, want 0", got)
	}

	mustExec(t, db, `
		UPDATE projection_meta
		SET value = (market_today() - 1)::TEXT
		WHERE key = 'last_effective_corporate_actions_date'
	`)
	mustExec(t, db, `SELECT refresh_effective_corporate_actions(market_today())`)

	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, rolloverRewardID); got != "5.000000" {
		t.Errorf("rollover adjusted_quantity = %s, want 5.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT total_adjustment_amount::TEXT FROM reward_state WHERE reward_id = $1
	`, rolloverRewardID); got != "2.0000" {
		t.Errorf("rollover total_adjustment_amount = %s, want 2.0000", got)
	}
}

// TestProjectionViewShapeIntegration verifies that 0027 preserved the exact
// public view columns, order, and types.
func TestProjectionViewShapeIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	expected := map[string][]string{
		"reward_corporate_action_outcomes": {
			"reward_id|integer||32|0",
			"user_id|integer||32|0",
			"original_symbol|character varying|20||",
			"terminal_symbol|text|||",
			"reward_date|date|||",
			"adjusted_quantity|numeric|||",
			"delist_event_id|integer||32|0",
			"delist_effective_date|date|||",
			"outcome_type|text|||",
			"cash_proceeds|numeric|||",
			"written_off_quantity|numeric|||",
			"total_adjustment_amount|numeric|||",
			"is_active|boolean|||",
			"cash_in_lieu_proceeds|numeric|||",
			"fraction_quantity|numeric|||",
		},
		"user_portfolio": {
			"user_id|integer||32|0",
			"stock_symbol|text|||",
			"adjusted_quantity|numeric|||",
			"current_price|numeric|||",
			"inr_value|numeric|||",
			"total_adjustment_amount|numeric|||",
			"price_status|text|||",
			"price_as_of|timestamp with time zone|||",
			"price_source|text|||",
			"price_fetched_at|timestamp with time zone|||",
		},
		"today_rewards": {
			"reward_event_id|integer||32|0",
			"user_id|integer||32|0",
			"stock_symbol|text|||",
			"adjusted_quantity|numeric|||",
			"current_price|numeric|||",
			"total_adjustment_amount|numeric|||",
			"inr_value|numeric|||",
			"price_status|text|||",
			"price_as_of|timestamp with time zone|||",
			"price_source|text|||",
			"price_fetched_at|timestamp with time zone|||",
		},
		"user_corporate_action_cash": {
			"user_id|integer||32|0",
			"cash_proceeds|numeric|||",
			"written_off_quantity|numeric|||",
			"cash_in_lieu_proceeds|numeric|||",
		},
		"stock_prices_adjusted": {
			"id|integer||32|0",
			"stock_symbol|character varying|20||",
			"raw_price|numeric||18|4",
			"adjusted_price|numeric|||",
			"updated_at|timestamp with time zone|||",
			"source|text|||",
			"fetched_at|timestamp with time zone|||",
			"provider_symbol|text|||",
			"exchange|text|||",
			"currency|text|||",
		},
	}

	for view, want := range expected {
		rows, err := db.Query(`
			SELECT column_name || '|' || data_type || '|' ||
			       COALESCE(character_maximum_length::TEXT, '') || '|' ||
			       COALESCE(numeric_precision::TEXT, '') || '|' ||
			       COALESCE(numeric_scale::TEXT, '')
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1
			ORDER BY ordinal_position
		`, view)
		if err != nil {
			t.Fatalf("read columns for %s: %v", view, err)
		}

		var got []string
		for rows.Next() {
			var shape string
			if err := rows.Scan(&shape); err != nil {
				rows.Close()
				t.Fatalf("scan columns for %s: %v", view, err)
			}
			got = append(got, shape)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate columns for %s: %v", view, err)
		}
		rows.Close()

		if len(got) != len(want) {
			t.Errorf("%s has %d columns, want %d: %v", view, len(got), len(want), got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s column %d = %s, want %s", view, i+1, got[i], want[i])
			}
		}
	}
}

// TestProjectionMigrationRoundTripIntegration verifies that 0026 down restores
// the 0022 stock-event trigger and that 0027 down restores computational views.
func TestProjectionMigrationRoundTripIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	if !triggerExists(t, db, "trg_process_corporate_action_cash_insert") {
		t.Fatal("0026 insert trigger is missing")
	}
	if relationExists(t, db, "reward_state") != true {
		t.Fatal("reward_state table is missing at version 27")
	}

	applyMigrations(t, dbURL, 25)

	if !triggerExists(t, db, "trg_process_corporate_action_cash") {
		t.Error("0026 down did not restore trg_process_corporate_action_cash")
	}
	if triggerExists(t, db, "trg_process_corporate_action_cash_insert") {
		t.Error("0026 down left the insert transition-table trigger behind")
	}
	if relationExists(t, db, "reward_state") {
		t.Error("0026 down left reward_state behind")
	}
	if relationExists(t, db, "reward_corporate_action_outcomes_base") {
		t.Error("0026 down left the private base view behind")
	}

	var body string
	if err := db.QueryRow(`
		SELECT prosrc FROM pg_proc WHERE proname = 'sync_corporate_action_cash_trigger'
	`).Scan(&body); err != nil {
		t.Fatalf("read restored trigger function: %v", err)
	}
	if body == "" {
		t.Error("restored sync_corporate_action_cash_trigger has an empty body")
	}

	applyMigrations(t, dbURL, -1)
	if !triggerExists(t, db, "trg_process_corporate_action_cash_insert") {
		t.Error("0026 did not re-apply the projection trigger")
	}
	if !relationExists(t, db, "reward_state") {
		t.Error("0026 did not re-create reward_state")
	}
}
