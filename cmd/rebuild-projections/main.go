// Command rebuild-projections populates the Phase 1 read projections from the
// authoritative base tables.
//
//	--online   fill missing rows only; safe while the API is live
//	--offline  repair every row; run with writes quiesced
//
// 0026 creates the projection tables and triggers but deliberately does not
// backfill 500k rewards inside the API startup migration. Run this command once
// after shipping 0026, verify parity, then ship 0027.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
	"github.com/sirupsen/logrus"

	"github.com/LoganX64/stocky-api/internal/config"
)

const rebuildLockID int64 = 810201711

type options struct {
	online      bool
	offline     bool
	batchSize   int
	userBatch   int
	skipVacuum  bool
	databaseURL string
}

func main() {
	var opts options
	flag.BoolVar(&opts.online, "online", false, "fill missing projection rows only (safe while the API is live)")
	flag.BoolVar(&opts.offline, "offline", false, "recompute and repair every projection row (quiesce writes first)")
	flag.IntVar(&opts.batchSize, "batch-size", 5000, "rewards per batch")
	flag.IntVar(&opts.userBatch, "user-batch-size", 1000, "users per aggregate batch")
	flag.BoolVar(&opts.skipVacuum, "skip-vacuum", false, "skip the final VACUUM ANALYZE")
	flag.StringVar(&opts.databaseURL, "database-url", "", "PostgreSQL URL; defaults to the application configuration")
	flag.Parse()

	if opts.online == opts.offline {
		fmt.Fprintln(os.Stderr, "exactly one of --online or --offline is required")
		os.Exit(2)
	}
	if opts.batchSize <= 0 || opts.userBatch <= 0 {
		fmt.Fprintln(os.Stderr, "--batch-size and --user-batch-size must be positive")
		os.Exit(2)
	}

	if err := run(opts); err != nil {
		logrus.WithError(err).Fatal("projection rebuild failed")
	}
}

func run(opts options) error {
	db, err := openDB(opts.databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logrus.WithError(err).Warn("Failed to close DB cleanly")
		}
	}()

	ctx := context.Background()
	mode := "online"
	if opts.offline {
		mode = "offline"
	}

	// One rebuild at a time. Keep the lock on a dedicated connection so it
	// survives the pool handing out different connections per batch.
	lockConn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire rebuild lock connection: %w", err)
	}
	defer lockConn.Close()

	var locked bool
	if err := lockConn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, rebuildLockID).Scan(&locked); err != nil {
		return fmt.Errorf("acquire rebuild lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("another projection rebuild is already running")
	}
	defer func() {
		if _, err := lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, rebuildLockID); err != nil {
			logrus.WithError(err).Warn("Failed to release rebuild lock")
		}
	}()

	started := time.Now()
	logrus.WithFields(logrus.Fields{
		"mode":       mode,
		"batch_size": opts.batchSize,
		"user_batch": opts.userBatch,
	}).Info("Projection rebuild started")

	if err := setMeta(ctx, db, "last_rebuild_started_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := setMeta(ctx, db, "last_rebuild_mode", mode); err != nil {
		return err
	}

	if err := rebuildInstrumentFactors(ctx, db, opts.offline); err != nil {
		return err
	}

	maxRewardID, err := maxID(ctx, db, "rewards")
	if err != nil {
		return err
	}
	rewardRows, err := rebuildRewardState(ctx, db, opts.offline, opts.batchSize, maxRewardID)
	if err != nil {
		return err
	}

	maxUserID, err := maxID(ctx, db, "users")
	if err != nil {
		return err
	}
	portfolioRows, cashRows, err := rebuildUserAggregates(ctx, db, opts.offline, opts.userBatch, maxUserID)
	if err != nil {
		return err
	}

	if !opts.skipVacuum {
		if err := vacuumAnalyze(ctx, db); err != nil {
			return err
		}
	}

	finished := time.Now()
	if err := setMeta(ctx, db, "last_rebuild_finished_at", finished.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if opts.offline {
		if err := setMeta(ctx, db, "last_full_rebuild_at", finished.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	} else {
		if err := setMeta(ctx, db, "last_online_backfill_at", finished.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}

	logrus.WithFields(logrus.Fields{
		"mode":             mode,
		"reward_rows":      rewardRows,
		"portfolio_rows":   portfolioRows,
		"cash_rows":        cashRows,
		"duration_seconds": finished.Sub(started).Seconds(),
	}).Info("Projection rebuild complete")
	return nil
}

func openDB(databaseURL string) (*sql.DB, error) {
	connStr := databaseURL
	if connStr == "" {
		cfg := config.MustLoad()
		connStr = fmt.Sprintf(
			"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
			cfg.Database.Host,
			cfg.Database.DbPort,
			cfg.Database.User,
			cfg.Database.Password,
			cfg.Database.DBName,
			cfg.Database.SSLMode,
		)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}
	return db, nil
}

func setMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO projection_meta (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value,
		    updated_at = NOW()
	`, key, value)
	if err != nil {
		return fmt.Errorf("set projection_meta %s: %w", key, err)
	}
	return nil
}

func maxID(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var value int64
	query := `SELECT COALESCE(MAX(id), 0) FROM ` + table
	if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return 0, fmt.Errorf("read max id from %s: %w", table, err)
	}
	return value, nil
}

func rebuildInstrumentFactors(ctx context.Context, db *sql.DB, offline bool) error {
	var count int64
	query := `SELECT backfill_instrument_factors(market_today())`
	if offline {
		query = `SELECT refresh_instrument_factors(market_today())`
	}
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return fmt.Errorf("refresh instrument_factor: %w", err)
	}
	logrus.WithFields(logrus.Fields{"offline": offline, "rows": count}).Info("Instrument factors rebuilt")
	return nil
}

func rebuildRewardState(ctx context.Context, db *sql.DB, offline bool, batchSize int, maxRewardID int64) (int64, error) {
	conflict := "DO NOTHING"
	if offline {
		conflict = `DO UPDATE SET
			user_id = EXCLUDED.user_id,
			original_symbol = EXCLUDED.original_symbol,
			terminal_symbol = EXCLUDED.terminal_symbol,
			reward_date = EXCLUDED.reward_date,
			adjusted_quantity = EXCLUDED.adjusted_quantity,
			total_adjustment_amount = EXCLUDED.total_adjustment_amount,
			outcome_type = EXCLUDED.outcome_type,
			is_active = EXCLUDED.is_active,
			delist_event_id = EXCLUDED.delist_event_id,
			delist_effective_date = EXCLUDED.delist_effective_date,
			cash_proceeds = EXCLUDED.cash_proceeds,
			written_off_quantity = EXCLUDED.written_off_quantity,
			cash_in_lieu_proceeds = EXCLUDED.cash_in_lieu_proceeds,
			fraction_quantity = EXCLUDED.fraction_quantity,
			updated_at = NOW()`
	}

	query := `
		INSERT INTO reward_state (
			reward_id,
			user_id,
			original_symbol,
			terminal_symbol,
			reward_date,
			adjusted_quantity,
			total_adjustment_amount,
			outcome_type,
			is_active,
			delist_event_id,
			delist_effective_date,
			cash_proceeds,
			written_off_quantity,
			cash_in_lieu_proceeds,
			fraction_quantity
		)
		SELECT
			o.reward_id,
			o.user_id,
			o.original_symbol::TEXT,
			o.terminal_symbol::TEXT,
			o.reward_date,
			o.adjusted_quantity::NUMERIC(18,6),
			o.total_adjustment_amount::NUMERIC(18,4),
			o.outcome_type::TEXT,
			o.is_active,
			o.delist_event_id,
			o.delist_effective_date,
			o.cash_proceeds::NUMERIC(18,4),
			o.written_off_quantity::NUMERIC(18,6),
			o.cash_in_lieu_proceeds::NUMERIC(18,4),
			o.fraction_quantity::NUMERIC(18,6)
		FROM rewards r
		CROSS JOIN LATERAL reward_corporate_action_outcome_base(r.id) o
		WHERE r.id >= $1 AND r.id < $2
		ON CONFLICT (reward_id) ` + conflict

	deleteStale := `
		DELETE FROM reward_state rs
		USING rewards r
		WHERE rs.reward_id = r.id
		  AND r.id >= $1 AND r.id < $2
		  AND r.reward_date > market_today()`

	var total int64
	for lo := int64(1); lo <= maxRewardID; lo += int64(batchSize) {
		hi := lo + int64(batchSize)
		rows, err := rebuildBatch(ctx, db, func(tx *sql.Tx) (int64, error) {
			result, err := tx.ExecContext(ctx, query, lo, hi)
			if err != nil {
				return 0, err
			}
			if offline {
				if _, err := tx.ExecContext(ctx, deleteStale, lo, hi); err != nil {
					return 0, err
				}
			}
			if err := setMetaTx(ctx, tx, "rebuild_progress_reward_state", fmt.Sprintf("%d", hi-1)); err != nil {
				return 0, err
			}
			return result.RowsAffected()
		})
		if err != nil {
			return total, fmt.Errorf("rebuild reward_state [%d,%d): %w", lo, hi, err)
		}
		total += rows
		logrus.WithFields(logrus.Fields{"from": lo, "to": hi, "rows": total}).Debug("reward_state batch complete")
	}
	logrus.WithField("rows", total).Info("reward_state rebuilt")
	return total, nil
}

func rebuildUserAggregates(ctx context.Context, db *sql.DB, offline bool, batchSize int, maxUserID int64) (int64, int64, error) {
	conflictPortfolio := "DO NOTHING"
	conflictCash := "DO NOTHING"
	if offline {
		conflictPortfolio = `DO UPDATE SET
			quantity = EXCLUDED.quantity,
			total_adjustment_amount = EXCLUDED.total_adjustment_amount,
			updated_at = NOW()`
		conflictCash = `DO UPDATE SET
			cash_proceeds = EXCLUDED.cash_proceeds,
			written_off_quantity = EXCLUDED.written_off_quantity,
			cash_in_lieu_proceeds = EXCLUDED.cash_in_lieu_proceeds,
			updated_at = NOW()`
	}

	portfolioQuery := `
		INSERT INTO portfolio_position (
			user_id, terminal_symbol, quantity, total_adjustment_amount, updated_at
		)
		SELECT
			user_id,
			terminal_symbol,
			SUM(adjusted_quantity),
			SUM(total_adjustment_amount),
			NOW()
		FROM reward_state
		WHERE user_id >= $1 AND user_id < $2
		  AND is_active
		GROUP BY user_id, terminal_symbol
		ON CONFLICT (user_id, terminal_symbol) ` + conflictPortfolio

	cashQuery := `
		INSERT INTO user_cash_position (
			user_id, cash_proceeds, written_off_quantity, cash_in_lieu_proceeds, updated_at
		)
		SELECT
			user_id,
			SUM(cash_proceeds),
			SUM(written_off_quantity),
			SUM(cash_in_lieu_proceeds),
			NOW()
		FROM reward_state
		WHERE user_id >= $1 AND user_id < $2
		GROUP BY user_id
		ON CONFLICT (user_id) ` + conflictCash

	deleteStalePortfolio := `
		DELETE FROM portfolio_position p
		WHERE p.user_id >= $1 AND p.user_id < $2
		  AND NOT EXISTS (
			SELECT 1
			FROM reward_state rs
			WHERE rs.user_id = p.user_id
			  AND rs.terminal_symbol = p.terminal_symbol
			  AND rs.is_active
		  )`

	deleteStaleCash := `
		DELETE FROM user_cash_position c
		WHERE c.user_id >= $1 AND c.user_id < $2
		  AND NOT EXISTS (
			SELECT 1 FROM reward_state rs WHERE rs.user_id = c.user_id
		  )`

	var portfolioTotal, cashTotal int64
	for lo := int64(1); lo <= maxUserID; lo += int64(batchSize) {
		hi := lo + int64(batchSize)
		var portfolioRows, cashRows int64
		_, err := rebuildBatch(ctx, db, func(tx *sql.Tx) (int64, error) {
			result, err := tx.ExecContext(ctx, portfolioQuery, lo, hi)
			if err != nil {
				return 0, err
			}
			portfolioRows, err = result.RowsAffected()
			if err != nil {
				return 0, err
			}
			result, err = tx.ExecContext(ctx, cashQuery, lo, hi)
			if err != nil {
				return 0, err
			}
			cashRows, err = result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if offline {
				if _, err := tx.ExecContext(ctx, deleteStalePortfolio, lo, hi); err != nil {
					return 0, err
				}
				if _, err := tx.ExecContext(ctx, deleteStaleCash, lo, hi); err != nil {
					return 0, err
				}
			}
			if err := setMetaTx(ctx, tx, "rebuild_progress_user_aggregates", fmt.Sprintf("%d", hi-1)); err != nil {
				return 0, err
			}
			return portfolioRows + cashRows, nil
		})
		if err != nil {
			return portfolioTotal, cashTotal, fmt.Errorf("rebuild user aggregates [%d,%d): %w", lo, hi, err)
		}
		portfolioTotal += portfolioRows
		cashTotal += cashRows
		logrus.WithFields(logrus.Fields{
			"from":      lo,
			"to":        hi,
			"portfolio": portfolioTotal,
			"cash":      cashTotal,
		}).Debug("user aggregate batch complete")
	}
	logrus.WithFields(logrus.Fields{"portfolio_rows": portfolioTotal, "cash_rows": cashTotal}).Info("User aggregates rebuilt")
	return portfolioTotal, cashTotal, nil
}

func rebuildBatch(ctx context.Context, db *sql.DB, fn func(*sql.Tx) (int64, error)) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := fn(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return rows, nil
}

func setMetaTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO projection_meta (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value,
		    updated_at = NOW()
	`, key, value)
	if err != nil {
		return fmt.Errorf("set projection_meta %s: %w", key, err)
	}
	return nil
}

func vacuumAnalyze(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		VACUUM (ANALYZE)
			reward_state,
			portfolio_position,
			user_cash_position,
			instrument_factor
	`)
	if err != nil {
		return fmt.Errorf("vacuum analyze projections: %w", err)
	}
	return nil
}
