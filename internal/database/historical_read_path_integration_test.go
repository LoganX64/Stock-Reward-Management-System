package database

import (
	"database/sql"
	"testing"
)

// historicalReadRow is one row from historical_rewards_for_user(). A
// summary-only row (an empty page) has NULL reward columns.
type historicalReadRow struct {
	RewardEventID    sql.NullInt64
	StockSymbol      sql.NullString
	AdjustedQuantity sql.NullString
	Price            sql.NullString
	INRValue         sql.NullString
	PriceStatus      sql.NullString
	UnpricedRewards  int64
	UnpricedSymbols  string
	ValuationDate    string
	AsOfAllowed      bool
}

// readHistoricalPage calls historical_rewards_for_user() with the same column
// list and ordering the API handler uses.
func readHistoricalPage(t *testing.T, db *sql.DB, userID int, asOf any, limit, offset int) []historicalReadRow {
	t.Helper()

	rows, err := db.Query(`
		SELECT
			reward_event_id,
			stock_symbol,
			adjusted_quantity::TEXT,
			price::TEXT,
			inr_value::TEXT,
			price_status,
			unpriced_rewards,
			unpriced_symbols::TEXT,
			valuation_date::TEXT,
			as_of_allowed
		FROM historical_rewards_for_user($1, $2::DATE, $3, $4)
		ORDER BY reward_date DESC NULLS LAST, reward_event_id DESC NULLS LAST
	`, userID, asOf, limit, offset)
	if err != nil {
		t.Fatalf("query historical_rewards_for_user: %v", err)
	}
	defer rows.Close()

	var result []historicalReadRow
	for rows.Next() {
		var row historicalReadRow
		if err := rows.Scan(
			&row.RewardEventID,
			&row.StockSymbol,
			&row.AdjustedQuantity,
			&row.Price,
			&row.INRValue,
			&row.PriceStatus,
			&row.UnpricedRewards,
			&row.UnpricedSymbols,
			&row.ValuationDate,
			&row.AsOfAllowed,
		); err != nil {
			t.Fatalf("scan historical_rewards_for_user row: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate historical_rewards_for_user rows: %v", err)
	}
	return result
}

// TestHistoricalReadPathIntegration covers the Phase 3 user-scoped historical
// function: page-first ordering, full-set unpriced summary on every page
// (including an empty one), grant-date vs as-of valuation, future-date
// rejection, and parity with the retained historical_rewards_as_of() function.
func TestHistoricalReadPathIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	userID := insertIntegrationUser(t, db, "historical_read_user")
	otherUserID := insertIntegrationUser(t, db, "historical_other_user")

	// created_at drives reward_date through trg_set_reward_date.
	insertReward := func(user int, symbol string, quantity int, daysAgo int) int {
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
		`, user, symbol, quantity, daysAgo).Scan(&id); err != nil {
			t.Fatalf("insert reward %s: %v", symbol, err)
		}
		return id
	}

	oldestID := insertReward(userID, "HIST", 10, 30)
	middleID := insertReward(userID, "MISSING", 5, 20)
	newestID := insertReward(userID, "HIST", 2, 10)
	insertReward(otherUserID, "HIST", 99, 5)

	// HIST has a price on each reward date. MISSING deliberately has none, so
	// the full-set summary must report it even when it is not on the page.
	mustExec(t, db, `
		INSERT INTO stock_price_history (stock_symbol, price, date, source, fetched_at)
		VALUES
			('HIST', 100, market_today() - 30, 'simulator', NOW()),
			('HIST', 100, market_today() - 20, 'simulator', NOW()),
			('HIST', 120, market_today() - 10, 'simulator', NOW())
	`)

	// A 2:1 split after the oldest grant but before the newest grant makes the
	// as-of quantity date-sensitive without affecting grant-date valuation.
	mustExec(t, db, `
		INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
		VALUES ('HIST', 'split', 2, 1, market_today() - 15)
	`)

	// Grant-date mode values original quantities at the reward-date price and
	// returns the two newest rewards first. The unpriced MISSING reward is
	// included because it is on this page.
	firstPage := readHistoricalPage(t, db, userID, nil, 2, 0)
	if len(firstPage) != 2 {
		t.Fatalf("grant-date first page has %d rows, want 2", len(firstPage))
	}
	if firstPage[0].RewardEventID.Int64 != int64(newestID) || firstPage[0].StockSymbol.String != "HIST" {
		t.Errorf("grant-date first row = id %d / %s, want id %d / HIST",
			firstPage[0].RewardEventID.Int64, firstPage[0].StockSymbol.String, newestID)
	}
	if firstPage[0].AdjustedQuantity.String != "2.000000" {
		t.Errorf("newest adjusted_quantity = %s, want 2.000000", firstPage[0].AdjustedQuantity.String)
	}
	if firstPage[0].Price.String != "60.0000000000000000" {
		t.Errorf("newest grant-date price = %s, want 60.0000000000000000", firstPage[0].Price.String)
	}
	if firstPage[0].INRValue.String != "120.0000" {
		t.Errorf("newest grant-date inr_value = %s, want 120.0000", firstPage[0].INRValue.String)
	}
	if firstPage[0].PriceStatus.String != "available" {
		t.Errorf("newest price_status = %s, want available", firstPage[0].PriceStatus.String)
	}

	if firstPage[1].RewardEventID.Int64 != int64(middleID) || firstPage[1].StockSymbol.String != "MISSING" {
		t.Errorf("grant-date second row = id %d / %s, want id %d / MISSING",
			firstPage[1].RewardEventID.Int64, firstPage[1].StockSymbol.String, middleID)
	}
	if firstPage[1].PriceStatus.String != "missing" {
		t.Errorf("MISSING price_status = %s, want missing", firstPage[1].PriceStatus.String)
	}
	if firstPage[1].Price.Valid || firstPage[1].INRValue.Valid {
		t.Errorf("MISSING price/inr_value should be NULL, got price=%v inr=%v",
			firstPage[1].Price, firstPage[1].INRValue)
	}

	for _, row := range firstPage {
		if row.UnpricedRewards != 1 {
			t.Errorf("page unpriced_rewards = %d, want 1", row.UnpricedRewards)
		}
		if row.UnpricedSymbols != "{MISSING}" {
			t.Errorf("page unpriced_symbols = %s, want {MISSING}", row.UnpricedSymbols)
		}
		if !row.AsOfAllowed {
			t.Error("grant-date as_of_allowed = false, want true")
		}
		if row.ValuationDate != scalarString(t, db, `SELECT market_today()::TEXT`) {
			t.Errorf("grant-date valuation_date = %s, want market_today()", row.ValuationDate)
		}
	}

	// The next page contains only the oldest reward.
	secondPage := readHistoricalPage(t, db, userID, nil, 2, 2)
	if len(secondPage) != 1 {
		t.Fatalf("grant-date second page has %d rows, want 1", len(secondPage))
	}
	if secondPage[0].RewardEventID.Int64 != int64(oldestID) {
		t.Errorf("grant-date second page id = %d, want %d", secondPage[0].RewardEventID.Int64, oldestID)
	}
	if secondPage[0].AdjustedQuantity.String != "10.000000" {
		t.Errorf("oldest grant-date adjusted_quantity = %s, want 10.000000", secondPage[0].AdjustedQuantity.String)
	}
	if secondPage[0].Price.String != "100.0000000000000000" {
		t.Errorf("oldest grant-date price = %s, want 100.0000000000000000", secondPage[0].Price.String)
	}

	// An empty page still returns the full-set summary, not an empty result.
	emptyPage := readHistoricalPage(t, db, userID, nil, 2, 10)
	if len(emptyPage) != 1 {
		t.Fatalf("empty page has %d summary rows, want 1", len(emptyPage))
	}
	if emptyPage[0].RewardEventID.Valid {
		t.Errorf("empty-page reward_event_id = %v, want NULL", emptyPage[0].RewardEventID)
	}
	if emptyPage[0].UnpricedRewards != 1 {
		t.Errorf("empty-page unpriced_rewards = %d, want 1", emptyPage[0].UnpricedRewards)
	}
	if emptyPage[0].UnpricedSymbols != "{MISSING}" {
		t.Errorf("empty-page unpriced_symbols = %s, want {MISSING}", emptyPage[0].UnpricedSymbols)
	}

	// The summary function itself always returns exactly one row.
	var (
		summaryDate     string
		summaryAllowed  bool
		summaryUnpriced int64
		summarySymbols  string
	)
	if err := db.QueryRow(`
		SELECT valuation_date::TEXT, as_of_allowed, unpriced_rewards, unpriced_symbols::TEXT
		FROM historical_unpriced_rewards_for_user($1, NULL::DATE)
	`, userID).Scan(&summaryDate, &summaryAllowed, &summaryUnpriced, &summarySymbols); err != nil {
		t.Fatalf("query historical_unpriced_rewards_for_user: %v", err)
	}
	if !summaryAllowed || summaryUnpriced != 1 || summarySymbols != "{MISSING}" {
		t.Errorf("summary = allowed %v / unpriced %d / %s, want true / 1 / {MISSING}",
			summaryAllowed, summaryUnpriced, summarySymbols)
	}
	if summaryDate != scalarString(t, db, `SELECT market_today()::TEXT`) {
		t.Errorf("summary valuation_date = %s, want market_today()", summaryDate)
	}

	// As of 20 days ago the split has not happened: the oldest reward still has
	// its original 10 shares and its -20 price. The newest reward is excluded.
	beforeSplit := readHistoricalPage(t, db, userID, scalarString(t, db, `SELECT (market_today() - 20)::TEXT`), 10, 0)
	if len(beforeSplit) != 2 {
		t.Fatalf("as-of -20 page has %d rows, want 2", len(beforeSplit))
	}
	if beforeSplit[0].RewardEventID.Int64 != int64(middleID) || beforeSplit[1].RewardEventID.Int64 != int64(oldestID) {
		t.Errorf("as-of -20 order = %d, %d; want %d, %d",
			beforeSplit[0].RewardEventID.Int64, beforeSplit[1].RewardEventID.Int64, middleID, oldestID)
	}
	if beforeSplit[1].AdjustedQuantity.String != "10.000000" {
		t.Errorf("as-of -20 oldest quantity = %s, want 10.000000", beforeSplit[1].AdjustedQuantity.String)
	}
	if beforeSplit[1].Price.String != "100.0000000000000000" {
		t.Errorf("as-of -20 oldest price = %s, want 100.0000000000000000", beforeSplit[1].Price.String)
	}
	if beforeSplit[1].INRValue.String != "1000.0000" {
		t.Errorf("as-of -20 oldest inr_value = %s, want 1000.0000", beforeSplit[1].INRValue.String)
	}
	if beforeSplit[0].PriceStatus.String != "missing" {
		t.Errorf("as-of -20 MISSING price_status = %s, want missing", beforeSplit[0].PriceStatus.String)
	}

	// As of 10 days ago the split has doubled the oldest quantity, and all three
	// rewards are in range.
	afterSplit := readHistoricalPage(t, db, userID, scalarString(t, db, `SELECT (market_today() - 10)::TEXT`), 10, 0)
	if len(afterSplit) != 3 {
		t.Fatalf("as-of -10 page has %d rows, want 3", len(afterSplit))
	}
	if afterSplit[0].RewardEventID.Int64 != int64(newestID) || afterSplit[1].RewardEventID.Int64 != int64(middleID) || afterSplit[2].RewardEventID.Int64 != int64(oldestID) {
		t.Errorf("as-of -10 order = %d, %d, %d; want %d, %d, %d",
			afterSplit[0].RewardEventID.Int64, afterSplit[1].RewardEventID.Int64, afterSplit[2].RewardEventID.Int64,
			newestID, middleID, oldestID)
	}
	if afterSplit[2].AdjustedQuantity.String != "20.000000" {
		t.Errorf("as-of -10 oldest quantity = %s, want 20.000000", afterSplit[2].AdjustedQuantity.String)
	}
	if afterSplit[2].Price.String != "60.0000000000000000" {
		t.Errorf("as-of -10 oldest price = %s, want 60.0000000000000000", afterSplit[2].Price.String)
	}
	if afterSplit[2].INRValue.String != "1200.0000" {
		t.Errorf("as-of -10 oldest inr_value = %s, want 1200.0000", afterSplit[2].INRValue.String)
	}
	for _, row := range afterSplit {
		if row.UnpricedRewards != 1 || row.UnpricedSymbols != "{MISSING}" {
			t.Errorf("as-of -10 summary = %d / %s, want 1 / {MISSING}",
				row.UnpricedRewards, row.UnpricedSymbols)
		}
	}

	// A future as-of is rejected by the function, not silently valued.
	future := scalarString(t, db, `SELECT (market_today() + 1)::TEXT`)
	futurePage := readHistoricalPage(t, db, userID, future, 10, 0)
	if len(futurePage) != 1 {
		t.Fatalf("future as-of page has %d rows, want 1 summary row", len(futurePage))
	}
	if futurePage[0].AsOfAllowed {
		t.Error("future as_of_allowed = true, want false")
	}
	if futurePage[0].RewardEventID.Valid {
		t.Errorf("future as-of reward_event_id = %v, want NULL", futurePage[0].RewardEventID)
	}
	if futurePage[0].UnpricedRewards != 0 {
		t.Errorf("future as-of unpriced_rewards = %d, want 0", futurePage[0].UnpricedRewards)
	}

	// The new page-first function must agree with the retained all-rewards
	// function in both modes.
	for _, asOf := range []any{nil, scalarString(t, db, `SELECT (market_today() - 10)::TEXT`)} {
		var mismatches int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM (
				(SELECT reward_event_id, stock_symbol, adjusted_quantity, price,
				        inr_value, price_status, outcome_type, is_active,
				        cash_proceeds, written_off_quantity
				 FROM historical_rewards_for_user($1, $2::DATE, 100, 0))
				EXCEPT
				(SELECT reward_event_id, stock_symbol, adjusted_quantity, price,
				        inr_value, price_status, outcome_type, is_active,
				        cash_proceeds, written_off_quantity
				 FROM historical_rewards_as_of($2::DATE)
				 WHERE user_id = $1
				   AND reward_date <= COALESCE($2::DATE, market_today() - 1))
				UNION ALL
				(SELECT reward_event_id, stock_symbol, adjusted_quantity, price,
				        inr_value, price_status, outcome_type, is_active,
				        cash_proceeds, written_off_quantity
				 FROM historical_rewards_as_of($2::DATE)
				 WHERE user_id = $1
				   AND reward_date <= COALESCE($2::DATE, market_today() - 1))
				EXCEPT
				(SELECT reward_event_id, stock_symbol, adjusted_quantity, price,
				        inr_value, price_status, outcome_type, is_active,
				        cash_proceeds, written_off_quantity
				 FROM historical_rewards_for_user($1, $2::DATE, 100, 0))
			) mismatches
		`, userID, asOf).Scan(&mismatches); err != nil {
			t.Fatalf("compare historical functions (asOf=%v): %v", asOf, err)
		}
		if mismatches != 0 {
			t.Errorf("historical function drift rows (asOf=%v) = %d, want 0", asOf, mismatches)
		}
	}
}
