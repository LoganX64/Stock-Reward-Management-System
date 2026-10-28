package main

import (
	"database/sql"
	"log"

	_ "github.com/lib/pq"
)

func main() {
	// Connect to DB
	db, err := sql.Open("postgres", "postgres://postgres:9908@localhost:5432/assignment?sslmode=disable")
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer db.Close()

	// Drop all views and corporate-action functions first
	dropViews := `
		DROP FUNCTION IF EXISTS backfill_price_history(DATE) CASCADE;
		DROP FUNCTION IF EXISTS process_corporate_action_cash(DATE) CASCADE;
		DROP FUNCTION IF EXISTS sync_corporate_action_cash_trigger() CASCADE;
		DROP FUNCTION IF EXISTS reward_corporate_action_cash_details(DATE) CASCADE;
		DROP FUNCTION IF EXISTS reward_action_settlement(INTEGER, DATE) CASCADE;
		DROP FUNCTION IF EXISTS reward_action_settlements(INTEGER, DATE) CASCADE;
		DROP FUNCTION IF EXISTS validate_stock_event_fraction() CASCADE;
		DROP FUNCTION IF EXISTS normalize_stock_event() CASCADE;
		DROP FUNCTION IF EXISTS instrument_delist_event(TEXT, DATE, DATE) CASCADE;
		DROP FUNCTION IF EXISTS reward_merger_cash_proceeds(INTEGER, DATE) CASCADE;
		DROP FUNCTION IF EXISTS reward_quantity_at(INTEGER, DATE) CASCADE;
		DROP FUNCTION IF EXISTS instrument_is_delisted(TEXT, DATE, DATE) CASCADE;
		DROP FUNCTION IF EXISTS instrument_quantity_factor(TEXT, DATE, DATE) CASCADE;
		DROP FUNCTION IF EXISTS instrument_terminal_symbol(TEXT, DATE, DATE) CASCADE;
		DROP FUNCTION IF EXISTS instrument_chain(TEXT, DATE, DATE) CASCADE;
		DROP VIEW IF EXISTS user_corporate_action_cash CASCADE;
		DROP VIEW IF EXISTS reward_corporate_action_outcomes CASCADE;
		DROP VIEW IF EXISTS instrument_lineage CASCADE;
		DROP VIEW IF EXISTS historical_rewards CASCADE;
		DROP VIEW IF EXISTS today_rewards CASCADE;
		DROP VIEW IF EXISTS user_portfolio CASCADE;
		DROP VIEW IF EXISTS reward_current_balances CASCADE;
		DROP VIEW IF EXISTS reward_quantity_movements CASCADE;
		DROP VIEW IF EXISTS reward_ledger_reconciliation CASCADE;
		DROP VIEW IF EXISTS stock_prices_adjusted CASCADE;
		DROP VIEW IF EXISTS stock_price_history_adjusted CASCADE;
	`
	_, err = db.Exec(dropViews)
	if err != nil {
		log.Fatal("Failed to drop views:", err)
	}

	// Drop all tables
	dropTables := `
		DROP TABLE IF EXISTS 
			schema_migrations,
			adjustments,
			ledger,
			rewards,
			stock_events,
			stock_prices,
			users,
			stock_price_history
		CASCADE;
	`
	_, err = db.Exec(dropTables)
	if err != nil {
		log.Fatal("Failed to drop tables:", err)
	}

	// Drop custom types
	dropTypes := `
		DO $$ 
		BEGIN
			DROP TYPE IF EXISTS ledger_entry_type CASCADE;
			DROP TYPE IF EXISTS stock_event_type CASCADE;
		EXCEPTION
			WHEN others THEN NULL;
		END $$;
	`
	_, err = db.Exec(dropTypes)
	if err != nil {
		log.Fatal("Failed to drop types:", err)
	}

	log.Println("Successfully dropped all database objects. Ready for fresh migrations.")
}
