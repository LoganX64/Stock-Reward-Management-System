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

	// Drop all views first
	dropViews := `
		DROP VIEW IF EXISTS historical_rewards CASCADE;
		DROP VIEW IF EXISTS today_rewards CASCADE;
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
