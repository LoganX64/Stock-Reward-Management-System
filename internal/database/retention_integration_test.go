// PostgreSQL integration tests for the financial-record retention and
// deletion policy.
//
// These tests are opt-in like the other integration tests:
//
//	TEST_DATABASE_URL='postgres://postgres:password@localhost:5432/postgres?sslmode=disable' \
//	  go test ./internal/database -run Retention -v -count=1
package database

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func TestRetentionDeletePolicyIntegration(t *testing.T) {
	adminURL := integrationDatabaseURL(t)
	dbURL := createIntegrationDatabase(t, adminURL)
	applyMigrations(t, dbURL, -1)

	db := openIntegrationDB(t, dbURL)

	userID := insertIntegrationUser(t, db, "retention-main")

	var rewardID int
	if err := db.QueryRow(`
		INSERT INTO rewards (user_id, stock_symbol, quantity, original_quantity, created_at)
		VALUES ($1, 'RETENTION', 10, 10,
		        (market_today() - 5)::timestamp AT TIME ZONE market_timezone())
		RETURNING id
	`, userID).Scan(&rewardID); err != nil {
		t.Fatalf("insert retention reward: %v", err)
	}

	mustExec(t, db, `
		INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount)
		VALUES ($1, 'stock_units', 'RETENTION', 10, 0)
	`, rewardID)
	mustExec(t, db, `
		INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount)
		VALUES ($1, 'inr_outflow', 'RETENTION', 0, -1000)
	`, rewardID)
	mustExec(t, db, `
		INSERT INTO adjustments (reward_id, adjustment_type, delta_quantity, delta_amount, reason)
		VALUES ($1, 'reward_reversal', -1, 0, 'retention test')
	`, rewardID)

	var eventID int
	if err := db.QueryRow(`
		INSERT INTO stock_events (stock_symbol, event_type, ratio_num, ratio_den, effective_date)
		VALUES ('RETENTIONEVT', 'split', 2, 1, market_today())
		RETURNING id
	`).Scan(&eventID); err != nil {
		t.Fatalf("insert retention stock event: %v", err)
	}

	mustExec(t, db, `
		INSERT INTO ledger (reward_id, stock_event_id, entry_type, stock_symbol, quantity, amount)
		VALUES ($1, $2, 'inr_outflow', 'RETENTION', 0, 25)
	`, rewardID, eventID)

	t.Run("user with reward history cannot be hard-deleted", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID)
		requireForeignKeyViolation(t, err, "rewards_user_id_fkey")

		if got := countRows(t, db, `SELECT COUNT(*) FROM users WHERE id = $1`, userID); got != 1 {
			t.Errorf("user count after rejected delete = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE id = $1`, rewardID); got != 1 {
			t.Errorf("reward count after rejected user delete = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 3 {
			t.Errorf("ledger count after rejected user delete = %d, want 3", got)
		}
	})

	t.Run("reward with financial children cannot be hard-deleted", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM rewards WHERE id = $1`, rewardID)
		requireForeignKeyViolation(t, err, "ledger_reward_id_fkey", "adjustments_reward_id_fkey")

		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE id = $1`, rewardID); got != 1 {
			t.Errorf("reward count after rejected delete = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 3 {
			t.Errorf("ledger count after rejected reward delete = %d, want 3", got)
		}
	})

	t.Run("stock event with tagged cash posting cannot be hard-deleted", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM stock_events WHERE id = $1`, eventID)
		requireForeignKeyViolation(t, err, "ledger_stock_event_id_fkey")

		if got := countRows(t, db, `SELECT COUNT(*) FROM stock_events WHERE id = $1`, eventID); got != 1 {
			t.Errorf("stock event count after rejected delete = %d, want 1", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE stock_event_id = $1`, eventID); got != 1 {
			t.Errorf("tagged ledger count after rejected event delete = %d, want 1", got)
		}
	})

	t.Run("deactivated user keeps history and cannot receive new rewards", func(t *testing.T) {
		mustExec(t, db, `UPDATE users SET deleted_at = NOW() WHERE id = $1`, userID)

		if !scalarBool(t, db, `SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, userID) {
			t.Error("user was not deactivated")
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 3 {
			t.Errorf("ledger count after deactivation = %d, want 3", got)
		}

		seedStockPrice(t, db, "RETENTIONNEW", 100)
		router := integrationRouter(db)
		response := doIntegrationRequest(t, router, http.MethodPost, "/api/v1/reward", map[string]any{
			"user_id":      userID,
			"stock_symbol": "RETENTIONNEW",
			"quantity":     1,
		})
		requireStatus(t, response, http.StatusBadRequest)
		if !strings.Contains(string(response.raw), "User does not exist") {
			t.Errorf("deactivated user reward response = %s, want User does not exist", response.raw)
		}

		history := doIntegrationRequest(t, router, http.MethodGet, "/api/v1/historical-inr/"+strconv.Itoa(userID), nil)
		requireStatus(t, history, http.StatusOK)
		if got := len(bodyArray(t, history.body, "history")); got != 1 {
			t.Errorf("historical reward count for deactivated user = %d, want 1", got)
		}
	})

	t.Run("down migration restores the previous cascade behavior", func(t *testing.T) {
		applyMigrations(t, dbURL, 23)

		if scalarBool(t, db, `
			SELECT EXISTS (
				SELECT 1
				FROM information_schema.columns
				WHERE table_schema = 'public'
				  AND table_name = 'users'
				  AND column_name = 'deleted_at'
			)
		`) {
			t.Error("users.deleted_at still exists after the down migration")
		}

		mustExec(t, db, `DELETE FROM users WHERE id = $1`, userID)

		if got := countRows(t, db, `SELECT COUNT(*) FROM rewards WHERE id = $1`, rewardID); got != 0 {
			t.Errorf("reward count after cascade restore = %d, want 0", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM ledger WHERE reward_id = $1`, rewardID); got != 0 {
			t.Errorf("ledger count after cascade restore = %d, want 0", got)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM adjustments WHERE reward_id = $1`, rewardID); got != 0 {
			t.Errorf("adjustment count after cascade restore = %d, want 0", got)
		}
	})
}

func requireForeignKeyViolation(t *testing.T, err error, constraints ...string) {
	t.Helper()

	if err == nil {
		t.Fatal("expected a foreign-key violation, got nil")
	}

	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("expected *pq.Error, got %T: %v", err, err)
	}
	if pqErr.Code != "23503" {
		t.Fatalf("SQLSTATE = %s, want 23503: %v", pqErr.Code, err)
	}
	for _, constraint := range constraints {
		if pqErr.Constraint == constraint {
			return
		}
	}
	t.Fatalf("foreign-key violation constraint = %q, want one of %v", pqErr.Constraint, constraints)
}
