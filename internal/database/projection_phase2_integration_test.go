package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func insertPhase2Reward(t *testing.T, db *sql.DB, userID int, symbol string, quantity int, daysAgo int) int {
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

func processOneQueueRow(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	var outcome string
	var lastErr sql.NullString
	if err := db.QueryRow(`
		SELECT outcome, last_error
		FROM process_projection_refresh_queue(5)
	`).Scan(&outcome, &lastErr); err != nil {
		t.Fatalf("process queue row: %v", err)
	}
	return outcome, lastErr.String
}

func TestProjectionPhase2QueueIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES ('ALPHA', 100.0000, 'simulator', NOW())
	`)

	userIDs := []int{
		insertIntegrationUser(t, db, "phase2_queue_1"),
		insertIntegrationUser(t, db, "phase2_queue_2"),
		insertIntegrationUser(t, db, "phase2_queue_3"),
	}
	for _, userID := range userIDs {
		insertPhase2Reward(t, db, userID, "ALPHA", 10, 30)
	}

	// Force a synthetic corporate action to exceed the synchronous threshold.
	mustExec(t, db, `
		UPDATE projection_meta
		SET value = '1'
		WHERE key = 'queue_sync_user_limit'
	`)

	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, effective_date
		)
		VALUES ('ALPHA', 'split', 2, 1, market_today() - 20)
	`)

	if got := countRows(t, db, `
		SELECT COUNT(*) FROM projection_refresh_queue
		WHERE processed_at IS NULL AND failed_at IS NULL
	`); got != 3 {
		t.Fatalf("pending queue rows = %d, want 3", got)
	}
	if got := countRows(t, db, `
		SELECT COUNT(*) FROM projection_reconciliation_focus
	`); got != 3 {
		t.Fatalf("focus rows = %d, want 3", got)
	}

	// reward_state is refreshed synchronously; the user aggregates wait for the
	// worker. This is the bounded fan-out contract.
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state
		WHERE user_id = $1
	`, userIDs[0]); got != "20.000000" {
		t.Errorf("queued reward_state quantity = %s, want 20.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT quantity::TEXT FROM portfolio_position
		WHERE user_id = $1 AND terminal_symbol = 'ALPHA'
	`, userIDs[0]); got != "10.000000" {
		t.Errorf("portfolio before drain = %s, want 10.000000", got)
	}

	for i := 0; i < 3; i++ {
		outcome, lastErr := processOneQueueRow(t, db)
		if outcome != "processed" {
			t.Fatalf("queue outcome %d = %s (%s), want processed", i, outcome, lastErr)
		}
	}
	if outcome, _ := processOneQueueRow(t, db); outcome != "idle" {
		t.Fatalf("empty queue outcome = %s, want idle", outcome)
	}

	for _, userID := range userIDs {
		if got := scalarString(t, db, `
			SELECT quantity::TEXT FROM portfolio_position
			WHERE user_id = $1 AND terminal_symbol = 'ALPHA'
		`, userID); got != "20.000000" {
			t.Errorf("user %d portfolio after drain = %s, want 20.000000", userID, got)
		}
	}

	// Event deletion must refresh both the factor and the projections. Use a
	// high threshold so this write is handled synchronously.
	mustExec(t, db, `
		UPDATE projection_meta
		SET value = '1000000'
		WHERE key = 'queue_sync_user_limit'
	`)
	mustExec(t, db, `
		DELETE FROM stock_events
		WHERE stock_symbol = 'ALPHA' AND event_type = 'split'
	`)

	if got := scalarString(t, db, `
		SELECT (factor = 1)::TEXT FROM instrument_factor WHERE stock_symbol = 'ALPHA'
	`); got != "true" {
		t.Errorf("factor after event delete = %s, want 1", got)
	}
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state
		WHERE user_id = $1
	`, userIDs[0]); got != "10.000000" {
		t.Errorf("reward_state after event delete = %s, want 10.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT quantity::TEXT FROM portfolio_position
		WHERE user_id = $1 AND terminal_symbol = 'ALPHA'
	`, userIDs[0]); got != "10.000000" {
		t.Errorf("portfolio after event delete = %s, want 10.000000", got)
	}
}

func TestProjectionPhase2IntermediateMergerIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES
			('CHAIN_A', 10.0000, 'simulator', NOW()),
			('CHAIN_B', 20.0000, 'simulator', NOW()),
			('CHAIN_C', 30.0000, 'simulator', NOW())
	`)

	userID := insertIntegrationUser(t, db, "phase2_chain_user")
	rewardA := insertPhase2Reward(t, db, userID, "CHAIN_A", 10, 60)
	rewardB := insertPhase2Reward(t, db, userID, "CHAIN_B", 5, 60)

	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, new_symbol, effective_date
		)
		VALUES ('CHAIN_A', 'merge', 1, 1, 'CHAIN_B', market_today() - 50)
	`)
	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, new_symbol, effective_date
		)
		VALUES ('CHAIN_B', 'merge', 1, 1, 'CHAIN_C', market_today() - 40)
	`)

	if got := scalarString(t, db, `
		SELECT terminal_symbol FROM reward_state WHERE reward_id = $1
	`, rewardA); got != "CHAIN_C" {
		t.Fatalf("chain terminal_symbol = %s, want CHAIN_C", got)
	}

	// A split on the intermediate symbol B must reach the reward that started
	// at A, because instrument_lineage walks backwards.
	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, effective_date
		)
		VALUES ('CHAIN_B', 'split', 2, 1, market_today() - 45)
	`)

	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, rewardA); got != "20.000000" {
		t.Errorf("road A quantity after intermediate split = %s, want 20.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, rewardB); got != "10.000000" {
		t.Errorf("road B quantity after intermediate split = %s, want 10.000000", got)
	}

	mustExec(t, db, `
		UPDATE stock_events
		SET ratio_num = 3
		WHERE stock_symbol = 'CHAIN_B' AND event_type = 'split'
	`)
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, rewardA); got != "30.000000" {
		t.Errorf("road A quantity after event update = %s, want 30.000000", got)
	}

	mustExec(t, db, `
		DELETE FROM stock_events
		WHERE stock_symbol = 'CHAIN_B' AND event_type = 'split'
	`)
	if got := scalarString(t, db, `
		SELECT adjusted_quantity::TEXT FROM reward_state WHERE reward_id = $1
	`, rewardA); got != "10.000000" {
		t.Errorf("road A quantity after event delete = %s, want 10.000000", got)
	}
	if got := scalarString(t, db, `
		SELECT quantity::TEXT FROM portfolio_position
		WHERE user_id = $1 AND terminal_symbol = 'CHAIN_C'
	`, userID); got != "15.000000" {
		t.Errorf("chain portfolio after delete = %s, want 15.000000", got)
	}
}

func TestProjectionPhase2ReconciliationIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES
			('RECON_A', 100.0000, 'simulator', NOW()),
			('RECON_B', 50.0000, 'simulator', NOW())
	`)
	userID := insertIntegrationUser(t, db, "phase2_recon_user")
	rewardA := insertPhase2Reward(t, db, userID, "RECON_A", 10, 30)
	insertPhase2Reward(t, db, userID, "RECON_B", 5, 30)

	// Cash-producing delist so the cash projection has non-zero values.
	mustExec(t, db, `
		INSERT INTO stock_events (
			stock_symbol, event_type, ratio_num, ratio_den, effective_date,
			delist_outcome, cash_per_unit
		)
		VALUES ('RECON_B', 'delist', 1, 1, market_today() - 20, 'cash_exit', 100)
	`)

	runReconciliation := func() (int, int, string) {
		t.Helper()
		var users, mismatches int
		var raw []byte
		if err := db.QueryRow(`
			SELECT users_checked, mismatch_count, mismatches
			FROM run_projection_reconciliation(0, 10, 100)
		`).Scan(&users, &mismatches, &raw); err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		return users, mismatches, string(raw)
	}

	// Reconciliation only sees focus users when the sample percent is zero.
	mustExec(t, db, `
		SELECT projection_touch_reconciliation_users(ARRAY[$1]::INTEGER[], 'test')
	`, userID)

	users, mismatches, raw := runReconciliation()
	if users != 1 || mismatches != 0 {
		t.Fatalf("clean reconciliation = %d users/%d mismatches (%s), want 1/0", users, mismatches, raw)
	}

	mustExec(t, db, `
		UPDATE portfolio_position
		SET quantity = quantity + 1
		WHERE user_id = $1 AND terminal_symbol = 'RECON_A'
	`, userID)
	mustExec(t, db, `
		SELECT projection_touch_reconciliation_users(ARRAY[$1]::INTEGER[], 'test')
	`, userID)
	users, mismatches, raw = runReconciliation()
	if users != 1 || mismatches == 0 || !strings.Contains(raw, "portfolio_diff") {
		t.Fatalf("portfolio drift = %d users/%d mismatches (%s), want portfolio_diff", users, mismatches, raw)
	}

	mustExec(t, db, `SELECT refresh_user_projections($1)`, userID)
	mustExec(t, db, `
		UPDATE reward_state
		SET adjusted_quantity = adjusted_quantity + 1
		WHERE reward_id = $1
	`, rewardA)
	mustExec(t, db, `
		SELECT projection_touch_reconciliation_users(ARRAY[$1]::INTEGER[], 'test')
	`, userID)
	users, mismatches, raw = runReconciliation()
	if users != 1 || mismatches == 0 || !strings.Contains(raw, "reward_state_diff") {
		t.Fatalf("reward drift = %d users/%d mismatches (%s), want reward_state_diff", users, mismatches, raw)
	}

	mustExec(t, db, `SELECT refresh_reward_state($1)`, rewardA)
	mustExec(t, db, `SELECT refresh_user_projections($1)`, userID)
	mustExec(t, db, `
		UPDATE user_cash_position
		SET cash_proceeds = cash_proceeds + 1
		WHERE user_id = $1
	`, userID)
	mustExec(t, db, `
		SELECT projection_touch_reconciliation_users(ARRAY[$1]::INTEGER[], 'test')
	`, userID)
	users, mismatches, raw = runReconciliation()
	if users != 1 || mismatches == 0 || !strings.Contains(raw, "cash_diff") {
		t.Fatalf("cash drift = %d users/%d mismatches (%s), want cash_diff", users, mismatches, raw)
	}

	mustExec(t, db, `SELECT refresh_user_projections($1)`, userID)
	mustExec(t, db, `
		SELECT projection_touch_reconciliation_users(ARRAY[$1]::INTEGER[], 'test')
	`, userID)
	users, mismatches, raw = runReconciliation()
	if users != 1 || mismatches != 0 {
		t.Fatalf("repaired reconciliation = %d users/%d mismatches (%s), want 1/0", users, mismatches, raw)
	}

	if got := scalarString(t, db, `
		SELECT value FROM projection_meta WHERE key = 'last_reconciliation_mismatches'
	`); got != "0" {
		t.Errorf("last_reconciliation_mismatches = %s, want 0", got)
	}
}

func TestProjectionPhase2QueueRetryIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES ('RETRY', 10.0000, 'simulator', NOW())
	`)
	userID := insertIntegrationUser(t, db, "phase2_retry_user")
	insertPhase2Reward(t, db, userID, "RETRY", 5, 30)

	mustExec(t, db, `
		SELECT projection_enqueue_users(ARRAY[$1]::INTEGER[], 'retry test')
	`, userID)

	ctx := context.Background()
	lockConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock($1::BIGINT)`, userID); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	defer lockConn.ExecContext(ctx, `SELECT pg_advisory_unlock($1::BIGINT)`, userID)

	workerConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open worker connection: %v", err)
	}
	defer workerConn.Close()
	if _, err := workerConn.ExecContext(ctx, `SET lock_timeout = '100ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}

	var outcome string
	var attempts int
	var lastErr sql.NullString
	var availableAt sql.NullTime
	if err := workerConn.QueryRowContext(ctx, `
		SELECT outcome, attempts, available_at, last_error
		FROM process_projection_refresh_queue(3)
	`).Scan(&outcome, &attempts, &availableAt, &lastErr); err != nil {
		t.Fatalf("process locked queue row: %v", err)
	}
	if outcome != "retry" || attempts != 1 {
		t.Fatalf("locked outcome = %s/%d, want retry/1", outcome, attempts)
	}
	if !lastErr.Valid || !strings.Contains(strings.ToLower(lastErr.String), "lock") {
		t.Fatalf("retry error = %q, want a lock timeout", lastErr.String)
	}

	var storedAttempts int
	var failedAt sql.NullTime
	if err := db.QueryRow(`
		SELECT attempts, failed_at FROM projection_refresh_queue
		WHERE user_id = $1 AND processed_at IS NULL AND failed_at IS NULL
	`, userID).Scan(&storedAttempts, &failedAt); err != nil {
		t.Fatalf("read retry state: %v", err)
	}
	if storedAttempts != 1 || failedAt.Valid {
		t.Errorf("stored retry state = %d/%v, want attempts 1 and no dead letter", storedAttempts, failedAt.Valid)
	}

	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_unlock($1::BIGINT)`, userID); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}
	if _, err := workerConn.ExecContext(ctx, `SET lock_timeout = 0`); err != nil {
		t.Fatalf("reset lock_timeout: %v", err)
	}
	mustExec(t, db, `
		UPDATE projection_refresh_queue
		SET available_at = NOW()
		WHERE user_id = $1 AND processed_at IS NULL AND failed_at IS NULL
	`, userID)

	if err := workerConn.QueryRowContext(ctx, `
		SELECT outcome FROM process_projection_refresh_queue(3)
	`).Scan(&outcome); err != nil {
		t.Fatalf("retry queue row: %v", err)
	}
	if outcome != "processed" {
		t.Fatalf("retry outcome = %s, want processed", outcome)
	}

	// A dead letter is visible and stops being claimed.
	mustExec(t, db, `
		SELECT projection_enqueue_users(ARRAY[$1]::INTEGER[], 'dead letter test')
	`, userID)
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock($1::BIGINT)`, userID); err != nil {
		t.Fatalf("reacquire advisory lock: %v", err)
	}
	if _, err := workerConn.ExecContext(ctx, `SET lock_timeout = '100ms'`); err != nil {
		t.Fatalf("re-set lock_timeout: %v", err)
	}
	if err := workerConn.QueryRowContext(ctx, `
		SELECT outcome FROM process_projection_refresh_queue(1)
	`).Scan(&outcome); err != nil {
		t.Fatalf("dead-letter queue row: %v", err)
	}
	if outcome != "failed" {
		t.Fatalf("dead-letter outcome = %s, want failed", outcome)
	}
	if got := countRows(t, db, `
		SELECT COUNT(*) FROM projection_refresh_queue WHERE failed_at IS NOT NULL
	`); got != 1 {
		t.Errorf("dead-letter rows = %d, want 1", got)
	}
}

func TestProjectionPhase2RolloverIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	if got := scalarInt(t, db, `SELECT refresh_instrument_factors_if_due(market_today())`); got < 0 {
		t.Errorf("first factor due check = %d, want a refresh", got)
	}
	if got := scalarInt(t, db, `SELECT refresh_instrument_factors_if_due(market_today())`); got != -1 {
		t.Errorf("second factor due check = %d, want -1", got)
	}
	if got := scalarInt(t, db, `SELECT refresh_effective_corporate_actions_if_due(market_today())`); got < 0 {
		t.Errorf("first effective due check = %d, want a refresh", got)
	}
	if got := scalarInt(t, db, `SELECT refresh_effective_corporate_actions_if_due(market_today())`); got != -1 {
		t.Errorf("second effective due check = %d, want -1", got)
	}
	if got := scalarInt(t, db, `SELECT process_corporate_action_cash_if_due(market_today())`); got < 0 {
		t.Errorf("first cash due check = %d, want a refresh", got)
	}
	if got := scalarInt(t, db, `SELECT process_corporate_action_cash_if_due(market_today())`); got != -1 {
		t.Errorf("second cash due check = %d, want -1", got)
	}

	// Factor health detects a stale row.
	mustExec(t, db, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES ('FACTOR', 10.0000, 'simulator', NOW())
	`)
	mustExec(t, db, `SELECT backfill_instrument_factors(market_today())`)
	mustExec(t, db, `
		UPDATE instrument_factor
		SET as_of = market_today() - 1
		WHERE stock_symbol = 'FACTOR'
	`)
	if got := scalarInt(t, db, `SELECT stale FROM projection_factor_status()`); got != 1 {
		t.Errorf("stale factor count = %d, want 1", got)
	}
	mustExec(t, db, `
		UPDATE projection_meta
		SET value = (market_today() - 1)::TEXT
		WHERE key = 'last_instrument_factor_date'
	`)
	mustExec(t, db, `SELECT refresh_instrument_factors_if_due(market_today())`)
	if got := scalarInt(t, db, `SELECT stale FROM projection_factor_status()`); got != 0 {
		t.Errorf("stale factor count after refresh = %d, want 0", got)
	}

	// Queue statistics and retention.
	userID := insertIntegrationUser(t, db, "phase2_stats_user")
	mustExec(t, db, `
		SELECT projection_enqueue_users(ARRAY[$1]::INTEGER[], 'stats test')
	`, userID)
	var pending, ready, failed int64
	if err := db.QueryRow(`
		SELECT pending, ready, failed FROM projection_queue_stats()
	`).Scan(&pending, &ready, &failed); err != nil {
		t.Fatalf("read queue stats: %v", err)
	}
	if pending != 1 || ready != 1 || failed != 0 {
		t.Errorf("queue stats = %d/%d/%d, want 1/1/0", pending, ready, failed)
	}
	if outcome, _ := processOneQueueRow(t, db); outcome != "processed" {
		t.Fatalf("queue outcome = %s, want processed", outcome)
	}
	mustExec(t, db, `
		UPDATE projection_refresh_queue
		SET processed_at = NOW() - INTERVAL '10 days'
		WHERE processed_at IS NOT NULL
	`)
	if got := scalarInt(t, db, `
		SELECT prune_projection_refresh_queue(INTERVAL '7 days', INTERVAL '30 days')
	`); got != 1 {
		t.Errorf("pruned rows = %d, want 1", got)
	}
}

func TestProjectionPhase2MigrationRoundTripIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)
	db := openIntegrationDB(t, dbURL)

	for _, column := range []string{"attempts", "last_error", "available_at", "failed_at"} {
		if !columnExists(t, db, "projection_refresh_queue", column) {
			t.Errorf("0028 queue column %s is missing", column)
		}
	}
	if !relationExists(t, db, "projection_reconciliation_focus") {
		t.Error("0028 focus table is missing")
	}
	for _, function := range []string{
		"projection_sync_user_limit",
		"process_projection_refresh_queue",
		"run_projection_reconciliation",
	} {
		if !functionExists(t, db, function) {
			t.Errorf("0028 function %s is missing", function)
		}
	}

	applyMigrations(t, dbURL, 27)

	if relationExists(t, db, "projection_reconciliation_focus") {
		t.Error("0028 down left the focus table behind")
	}
	for _, column := range []string{"attempts", "last_error", "available_at", "failed_at"} {
		if columnExists(t, db, "projection_refresh_queue", column) {
			t.Errorf("0028 down left queue column %s behind", column)
		}
	}
	if functionExists(t, db, "projection_sync_user_limit") {
		t.Error("0028 down left projection_sync_user_limit behind")
	}
	var body string
	if err := db.QueryRow(`
		SELECT prosrc FROM pg_proc WHERE proname = 'sync_corporate_action_cash_trigger'
	`).Scan(&body); err != nil {
		t.Fatalf("read restored trigger body: %v", err)
	}
	if strings.Contains(body, "projection_enqueue_users") {
		t.Error("0028 down did not restore the Phase 1 synchronous trigger")
	}

	applyMigrations(t, dbURL, -1)
	if !columnExists(t, db, "projection_refresh_queue", "attempts") {
		t.Error("0028 re-apply did not restore the queue columns")
	}
	if !relationExists(t, db, "projection_reconciliation_focus") {
		t.Error("0028 re-apply did not restore the focus table")
	}
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	return scalarBool(t, db, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name = $1
			  AND column_name = $2
		)
	`, table, column)
}

func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var value int
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("query scalar integer: %v", err)
	}
	return value
}
