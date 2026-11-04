// Command dbtool performs the database-side work for the benchmark harness,
// including the Phase 2 write-overhead measurement and live rebuild check.
//
// It uses the same configuration loader as the API, so it never needs the
// password to be copied into a shell environment or printed. It only reports
// connection success/failure and never logs credentials.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LoganX64/stocky-api/internal/config"
	"github.com/lib/pq"
)

var validDatabaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func main() {
	database := flag.String("database", "stocky_bench", "benchmark database name")
	host := flag.String("host", "", "override database host")
	port := flag.String("port", "", "override database port")
	user := flag.String("user", "", "override database user")
	seedFile := flag.String("seed-file", "bench/seed/seed_bench.sql", "seed SQL file")
	queryDir := flag.String("query-dir", "bench/queries", "directory of EXPLAIN query files")
	out := flag.String("out", "", "write output to this file instead of stdout")
	userID := flag.Int("user-id", 1, "user ID used by EXPLAIN queries")
	pageLimit := flag.Int("limit", 100, "page limit used by EXPLAIN queries")
	pageOffset := flag.Int("offset", 0, "page offset used by EXPLAIN queries")
	explainTimeout := flag.Duration("explain-timeout", 10*time.Minute, "timeout for each EXPLAIN statement")
	users := flag.Int("users", 20000, "seed users")
	rewardsPerUser := flag.Int("rewards-per-user", 25, "seed rewards per user")
	symbols := flag.Int("symbols", 3000, "seed instruments")
	historyDays := flag.Int("history-days", 60, "seed price-history days")
	recreate := flag.Bool("recreate", false, "drop and recreate the benchmark database during setup")
	service := flag.String("service", "postgresql-x64-17", "PostgreSQL Windows service name for enable-stats")
	iterations := flag.Int("iterations", 50, "measured iterations for write-overhead")
	warmup := flag.Int("warmup", 5, "warm-up iterations for write-overhead")
	rebuildBin := flag.String("rebuild-bin", "", "rebuild-projections binary for rebuild-check (default bench/bin/rebuild-projections[.exe])")
	rebuildRuns := flag.Int("rebuild-runs", 2, "online rebuild runs for rebuild-check")
	writers := flag.Int("writers", 4, "concurrent writers during rebuild-check")
	writeInterval := flag.Duration("write-interval", 500*time.Millisecond, "delay between concurrent writes during rebuild-check")
	writeLimit := flag.Int("write-limit", 250, "maximum rewards each concurrent writer inserts")
	reconcileSamplePercent := flag.Float64("sample-percent", 0.1, "reconciliation sample percent")
	reconcileMaxUsers := flag.Int("max-users", 500, "maximum users to reconcile")
	reconcileMaxDetails := flag.Int("max-details", 1000, "maximum mismatch details to retain")
	reconcileTimeout := flag.Duration("reconcile-timeout", 30*time.Minute, "reconciliation timeout")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		fmt.Fprintln(os.Stderr, "usage: dbtool <setup|seed|explain|stats|settings|activity|sessions|stop-sessions|enable-stats|disable-stats|reset-stats|counts|write-overhead|rebuild-check|reconcile> [flags]")
		os.Exit(2)
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fatal("load config: %v", err)
	}
	if *host != "" {
		cfg.Database.Host = *host
	}
	if *port != "" {
		cfg.Database.DbPort = *port
	}
	if *user != "" {
		cfg.Database.User = *user
	}
	if *database != "" {
		cfg.Database.DBName = *database
	}
	if !validDatabaseName.MatchString(cfg.Database.DBName) {
		fatal("invalid database name %q", cfg.Database.DBName)
	}

	switch command {
	case "setup":
		if err := setup(cfg.Database, *recreate); err != nil {
			fatal("setup: %v", err)
		}
	case "seed":
		if err := seed(cfg.Database, *seedFile, *users, *rewardsPerUser, *symbols, *historyDays); err != nil {
			fatal("seed: %v", err)
		}
	case "explain":
		if err := explain(cfg.Database, *queryDir, *out, *userID, *pageLimit, *pageOffset, *explainTimeout); err != nil {
			fatal("explain: %v", err)
		}
	case "stats":
		if err := stats(cfg.Database, *out); err != nil {
			fatal("stats: %v", err)
		}
	case "settings":
		if err := settings(cfg.Database, *out); err != nil {
			fatal("settings: %v", err)
		}
	case "activity":
		if err := activity(cfg.Database, *out); err != nil {
			fatal("activity: %v", err)
		}
	case "sessions":
		if err := sessions(cfg.Database, *out); err != nil {
			fatal("sessions: %v", err)
		}
	case "stop-sessions":
		if err := stopSessions(cfg.Database); err != nil {
			fatal("stop-sessions: %v", err)
		}
	case "enable-stats":
		if err := enableStats(cfg.Database, *service); err != nil {
			fatal("enable-stats: %v", err)
		}
	case "disable-stats":
		if err := disableStats(cfg.Database); err != nil {
			fatal("disable-stats: %v", err)
		}
	case "reset-stats":
		if err := resetStats(cfg.Database); err != nil {
			fatal("reset-stats: %v", err)
		}
	case "counts":
		if err := counts(cfg.Database, *out); err != nil {
			fatal("counts: %v", err)
		}
	case "write-overhead":
		if err := writeOverhead(cfg.Database, *iterations, *warmup, *out); err != nil {
			fatal("write-overhead: %v", err)
		}
	case "rebuild-check":
		if err := rebuildCheck(cfg.Database, *rebuildBin, *rebuildRuns, *writers, *writeInterval, *writeLimit, *out); err != nil {
			fatal("rebuild-check: %v", err)
		}
	case "reconcile":
		if err := reconcile(cfg.Database, *reconcileSamplePercent, *reconcileMaxUsers, *reconcileMaxDetails, *reconcileTimeout, *out); err != nil {
			fatal("reconcile: %v", err)
		}
	default:
		fatal("unknown command %q", command)
	}
}

func setup(dbConfig config.Database, recreate bool) error {
	admin, err := open(dbConfig, "postgres", 2)
	if err != nil {
		return err
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var exists int
	err = admin.QueryRowContext(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", dbConfig.DBName).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check database: %w", err)
	}
	if recreate && (errors.Is(err, sql.ErrNoRows) || exists == 1) {
		statement := `DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(dbConfig.DBName) + ` WITH (FORCE)`
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("drop database %s: %w", dbConfig.DBName, err)
		}
		exists = 0
		err = sql.ErrNoRows
		fmt.Printf("dropped database %s\n", dbConfig.DBName)
	}
	if errors.Is(err, sql.ErrNoRows) || exists != 1 {
		statement := `CREATE DATABASE ` + pq.QuoteIdentifier(dbConfig.DBName)
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create database %s: %w", dbConfig.DBName, err)
		}
		fmt.Printf("created database %s\n", dbConfig.DBName)
	} else {
		fmt.Printf("database %s already exists\n", dbConfig.DBName)
	}

	target, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer target.Close()

	var preload string
	if err := target.QueryRowContext(ctx, "SHOW shared_preload_libraries").Scan(&preload); err != nil {
		return fmt.Errorf("read shared_preload_libraries: %w", err)
	}
	fmt.Printf("shared_preload_libraries=%q\n", preload)

	if _, err := target.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		fmt.Printf("warning: could not create pg_stat_statements: %v\n", err)
	} else {
		fmt.Println("pg_stat_statements extension is available")
	}

	return nil
}

func enableStats(dbConfig config.Database, service string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}

	var current string
	if err := db.QueryRowContext(ctx, "SHOW shared_preload_libraries").Scan(&current); err != nil {
		db.Close()
		return fmt.Errorf("read shared_preload_libraries: %w", err)
	}
	fmt.Printf("shared_preload_libraries=%q\n", current)

	preloaded := false
	for _, part := range strings.Split(current, ",") {
		if strings.TrimSpace(part) == "pg_stat_statements" {
			preloaded = true
			break
		}
	}

	if preloaded {
		db.Close()
	} else {
		newValue := "pg_stat_statements"
		if trimmed := strings.TrimSpace(current); trimmed != "" {
			newValue = trimmed + ",pg_stat_statements"
		}
		statement := fmt.Sprintf(
			"ALTER SYSTEM SET shared_preload_libraries = %s",
			pq.QuoteLiteral(newValue),
		)
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return fmt.Errorf("ALTER SYSTEM: %w", err)
		}
		db.Close()
		fmt.Printf("set shared_preload_libraries=%q\n", newValue)

		if runtime.GOOS != "windows" {
			return fmt.Errorf("restart PostgreSQL to apply shared_preload_libraries, then rerun enable-stats")
		}
		if err := restartWindowsService(service); err != nil {
			return err
		}
	}

	// Wait for the server to accept connections again after the restart.
	deadline := time.Now().Add(60 * time.Second)
	for {
		db, err = open(dbConfig, dbConfig.DBName, 2)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PostgreSQL did not become ready: %w", err)
		}
		time.Sleep(time.Second)
	}
	defer db.Close()

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer readyCancel()

	var loaded string
	if err := db.QueryRowContext(readyCtx, "SHOW shared_preload_libraries").Scan(&loaded); err != nil {
		return fmt.Errorf("read shared_preload_libraries: %w", err)
	}
	if _, err := db.ExecContext(readyCtx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		return fmt.Errorf("create pg_stat_statements extension: %w", err)
	}
	fmt.Printf("shared_preload_libraries=%q\n", loaded)
	fmt.Println("pg_stat_statements is enabled")
	return nil
}

// disableStats removes the pg_stat_statements override written by
// enable-stats. A restart is required before the change takes effect.
func disableStats(dbConfig config.Database) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, "ALTER SYSTEM RESET shared_preload_libraries"); err != nil {
		return fmt.Errorf("ALTER SYSTEM RESET: %w", err)
	}
	fmt.Println("reset shared_preload_libraries; restart PostgreSQL to apply")
	return nil
}

// restartWindowsService uses the Windows service control commands, which
// require an elevated shell. "net stop" blocks until the service has stopped;
// /y also stops dependent services without prompting.
func restartWindowsService(name string) error {
	fmt.Printf("restarting service %s (requires an elevated shell)...\n", name)
	for _, args := range [][]string{{"stop", name, "/y"}, {"start", name}} {
		cmd := exec.Command("net", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("net %s %s: %w (run from an elevated shell)", args[0], name, err)
		}
	}
	return nil
}

func seed(dbConfig config.Database, path string, users, rewardsPerUser, symbols, historyDays int) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	text := stripPsqlMeta(string(raw))
	replacer := strings.NewReplacer(
		":rewards_per_user", fmt.Sprintf("%d", rewardsPerUser),
		":history_days", fmt.Sprintf("%d", historyDays),
		":symbols", fmt.Sprintf("%d", symbols),
		":users", fmt.Sprintf("%d", users),
	)
	text = replacer.Replace(text)

	db, err := open(dbConfig, dbConfig.DBName, 4)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	statements := splitStatements(text)
	start := time.Now()

	// The seed file owns BEGIN/COMMIT so it can also be run with psql. When
	// executing through database/sql, use one explicit transaction instead;
	// otherwise the pool is free to hand each statement a different connection.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed transaction: %w", err)
	}
	defer tx.Rollback()

	executed := 0
	for i, statement := range statements {
		trimmed := strings.TrimSpace(statement)
		if strings.EqualFold(trimmed, "BEGIN") || strings.EqualFold(trimmed, "COMMIT") {
			continue
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
		executed++
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed transaction: %w", err)
	}
	fmt.Printf("seed complete: %d statements in %s\n", executed, time.Since(start).Round(time.Millisecond))
	return nil
}

func explain(dbConfig config.Database, dir, outPath string, userID, limit, offset int, perStatementTimeout time.Duration) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no .sql files found in %s", dir)
	}

	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	var builder strings.Builder
	fmt.Fprintf(&builder, "Stocky query plans\nDatabase: %s\n", dbConfig.DBName)

	var failures []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		fmt.Fprintf(&builder, "\n===== %s =====\n", filepath.Base(file))

		text := stripPsqlMeta(string(raw))
		replacer := strings.NewReplacer(
			":page_limit", fmt.Sprintf("%d", limit),
			":page_offset", fmt.Sprintf("%d", offset),
			":user_id", fmt.Sprintf("%d", userID),
		)
		text = replacer.Replace(text)

		for index, statement := range splitStatements(text) {
			label := fmt.Sprintf("%s statement %d", filepath.Base(file), index+1)
			fmt.Fprintf(os.Stderr, "explain: %s (timeout %s)...\n", label, perStatementTimeout)
			started := time.Now()

			if err := collectPlan(db, statement, perStatementTimeout, &builder); err != nil {
				elapsed := time.Since(started).Round(time.Millisecond)
				fmt.Fprintf(os.Stderr, "explain: %s failed after %s: %v\n", label, elapsed, err)
				fmt.Fprintf(&builder, "ERROR after %s: %v\n", elapsed, err)
				failures = append(failures, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			fmt.Fprintf(os.Stderr, "explain: %s done in %s\n", label, time.Since(started).Round(time.Millisecond))
		}
	}

	if err := writeOutput(outPath, builder.String()); err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d plan(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func collectPlan(db *sql.DB, statement string, timeout time.Duration, builder *strings.Builder) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("scan plan: %w", err)
		}
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return rows.Err()
}

func stats(dbConfig config.Database, outPath string) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT
			calls,
			ROUND(total_exec_time::numeric, 2) AS total_exec_ms,
			ROUND(mean_exec_time::numeric, 2) AS mean_exec_ms,
			rows,
			ROUND(shared_blks_hit::numeric / NULLIF(shared_blks_hit + shared_blks_read, 0) * 100, 2) AS cache_hit_pct,
			LEFT(query, 200) AS query
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		ORDER BY total_exec_time DESC
		LIMIT 30`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("calls\ttotal_exec_ms\tmean_exec_ms\trows\tcache_hit_pct\tquery\n")
	for rows.Next() {
		var calls, rowCount int64
		var total, mean, cache sql.NullFloat64
		var query string
		if err := rows.Scan(&calls, &total, &mean, &rowCount, &cache, &query); err != nil {
			return err
		}
		fmt.Fprintf(&builder, "%d\t%.2f\t%.2f\t%d\t%.2f\t%s\n",
			calls, total.Float64, mean.Float64, rowCount, cache.Float64, strings.ReplaceAll(query, "\n", " "))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeOutput(outPath, builder.String())
}

func resetStats(dbConfig config.Database) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
		return err
	}
	fmt.Println("pg_stat_statements reset")
	return nil
}

func settings(dbConfig config.Database, outPath string) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var databaseName, version, preload, sharedBuffers, maxConnections, workMem string
	err = db.QueryRowContext(ctx, `
		SELECT
			current_database()::text,
			current_setting('server_version'),
			current_setting('shared_preload_libraries'),
			current_setting('shared_buffers'),
			current_setting('max_connections'),
			current_setting('work_mem')`).Scan(
		&databaseName, &version, &preload, &sharedBuffers, &maxConnections, &workMem,
	)
	if err != nil {
		return err
	}

	content := fmt.Sprintf(
		"database\t%s\nserver_version\t%s\nshared_preload_libraries\t%s\nshared_buffers\t%s\nmax_connections\t%s\nwork_mem\t%s\n",
		databaseName, version, preload, sharedBuffers, maxConnections, workMem,
	)
	return writeOutput(outPath, content)
}

func activity(dbConfig config.Database, outPath string) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(state, 'unknown') AS state, COUNT(*) AS connections
		FROM pg_stat_activity
		WHERE datname = current_database()
		GROUP BY state
		ORDER BY connections DESC, state`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("state\tconnections\n")
	for rows.Next() {
		var state string
		var connections int64
		if err := rows.Scan(&state, &connections); err != nil {
			return err
		}
		fmt.Fprintf(&builder, "%s\t%d\n", state, connections)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeOutput(outPath, builder.String())
}

func sessions(dbConfig config.Database, outPath string) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT
			pid,
			COALESCE(application_name, '') AS application_name,
			COALESCE(state, 'unknown') AS state,
			COALESCE(query_start::text, '') AS query_start,
			COALESCE(LEFT(REPLACE(query, E'\n', ' '), 160), '') AS query
		FROM pg_stat_activity
		WHERE datname = current_database()
		ORDER BY pid`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("pid\tapplication_name\tstate\tquery_start\tquery\n")
	for rows.Next() {
		var pid int
		var applicationName, state, queryStart, query string
		if err := rows.Scan(&pid, &applicationName, &state, &queryStart, &query); err != nil {
			return err
		}
		fmt.Fprintf(&builder, "%d\t%s\t%s\t%s\t%s\n", pid, applicationName, state, queryStart, query)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeOutput(outPath, builder.String())
}

func stopSessions(dbConfig config.Database) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var terminated int
	err = db.QueryRowContext(ctx, `
		WITH target AS (
			SELECT pid
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
		)
		SELECT COUNT(*)
		FROM target, LATERAL pg_terminate_backend(target.pid)`).Scan(&terminated)
	if err != nil {
		return err
	}
	fmt.Printf("terminated %d session(s) on %s\n", terminated, dbConfig.DBName)
	return nil
}

func counts(dbConfig config.Database, outPath string) error {
	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT 'users', COUNT(*) FROM users
		UNION ALL SELECT 'rewards', COUNT(*) FROM rewards
		UNION ALL SELECT 'adjustments', COUNT(*) FROM adjustments
		UNION ALL SELECT 'stock_prices', COUNT(*) FROM stock_prices
		UNION ALL SELECT 'stock_price_history', COUNT(*) FROM stock_price_history
		UNION ALL SELECT 'stock_events', COUNT(*) FROM stock_events
		UNION ALL SELECT 'ledger', COUNT(*) FROM ledger
		UNION ALL SELECT 'reward_state', COUNT(*) FROM reward_state
		UNION ALL SELECT 'portfolio_position', COUNT(*) FROM portfolio_position
		UNION ALL SELECT 'user_cash_position', COUNT(*) FROM user_cash_position
		UNION ALL SELECT 'instrument_factor', COUNT(*) FROM instrument_factor
		UNION ALL SELECT 'projection_refresh_queue', COUNT(*) FROM projection_refresh_queue
		UNION ALL SELECT 'projection_reconciliation_focus', COUNT(*) FROM projection_reconciliation_focus
		ORDER BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("relation\trows\n")
	for rows.Next() {
		var relation string
		var count int64
		if err := rows.Scan(&relation, &count); err != nil {
			return err
		}
		fmt.Fprintf(&builder, "%s\t%d\n", relation, count)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeOutput(outPath, builder.String())
}

// reconcile runs the production projection reconciliation function over a
// larger sample than the background service uses by default. A non-zero
// mismatch count is returned as an error so a scripted Phase 6 run fails loudly.
func reconcile(dbConfig config.Database, samplePercent float64, maxUsers, maxDetails int, timeout time.Duration, outPath string) error {
	if samplePercent <= 0 || samplePercent > 100 {
		return fmt.Errorf("-sample-percent must be greater than 0 and at most 100")
	}
	if maxUsers < 1 {
		return fmt.Errorf("-max-users must be at least 1")
	}
	if maxDetails < 0 {
		return fmt.Errorf("-max-details must not be negative")
	}
	if timeout <= 0 {
		return fmt.Errorf("-reconcile-timeout must be greater than zero")
	}

	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	started := time.Now()
	var usersChecked, mismatchCount int
	var raw []byte
	err = db.QueryRowContext(ctx, `
		SELECT users_checked, mismatch_count, mismatches
		FROM run_projection_reconciliation($1, $2, $3)
	`, samplePercent, maxUsers, maxDetails).Scan(&usersChecked, &mismatchCount, &raw)
	if err != nil {
		return err
	}
	elapsed := time.Since(started)

	var mismatches []json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &mismatches); err != nil {
			return fmt.Errorf("decode reconciliation mismatches: %w", err)
		}
	}

	var queuePending, queueReady, queueFailed, queueProcessed, oldestPendingSeconds int64
	var queueMaxAttempts int
	queueErr := db.QueryRowContext(ctx, `
		SELECT pending, ready, failed, processed, oldest_pending_seconds, max_attempts
		FROM projection_queue_stats()
	`).Scan(&queuePending, &queueReady, &queueFailed, &queueProcessed, &oldestPendingSeconds, &queueMaxAttempts)

	var factorTotal, factorStale, factorInvalid int64
	factorErr := db.QueryRowContext(ctx, `
		SELECT total, stale, invalid
		FROM projection_factor_status()
	`).Scan(&factorTotal, &factorStale, &factorInvalid)

	var builder strings.Builder
	fmt.Fprintf(&builder, "database\t%s\n", dbConfig.DBName)
	fmt.Fprintf(&builder, "sample_percent\t%g\n", samplePercent)
	fmt.Fprintf(&builder, "max_users\t%d\n", maxUsers)
	fmt.Fprintf(&builder, "max_details\t%d\n", maxDetails)
	fmt.Fprintf(&builder, "users_checked\t%d\n", usersChecked)
	fmt.Fprintf(&builder, "mismatch_count\t%d\n", mismatchCount)
	fmt.Fprintf(&builder, "duration_ms\t%d\n", elapsed.Milliseconds())

	if queueErr == nil {
		fmt.Fprintf(&builder, "queue_pending\t%d\n", queuePending)
		fmt.Fprintf(&builder, "queue_ready\t%d\n", queueReady)
		fmt.Fprintf(&builder, "queue_failed\t%d\n", queueFailed)
		fmt.Fprintf(&builder, "queue_processed\t%d\n", queueProcessed)
		fmt.Fprintf(&builder, "queue_oldest_pending_seconds\t%d\n", oldestPendingSeconds)
		fmt.Fprintf(&builder, "queue_max_attempts\t%d\n", queueMaxAttempts)
	} else {
		fmt.Fprintf(&builder, "queue_status_error\t%v\n", queueErr)
	}

	if factorErr == nil {
		fmt.Fprintf(&builder, "factor_total\t%d\n", factorTotal)
		fmt.Fprintf(&builder, "factor_stale\t%d\n", factorStale)
		fmt.Fprintf(&builder, "factor_invalid\t%d\n", factorInvalid)
	} else {
		fmt.Fprintf(&builder, "factor_status_error\t%v\n", factorErr)
	}

	for index, detail := range mismatches {
		var compact bytes.Buffer
		if err := json.Compact(&compact, detail); err != nil {
			fmt.Fprintf(&builder, "mismatch_%d\t%s\n", index+1, string(detail))
			continue
		}
		fmt.Fprintf(&builder, "mismatch_%d\t%s\n", index+1, compact.String())
	}
	if mismatchCount > len(mismatches) {
		fmt.Fprintf(&builder, "mismatch_details_truncated\t%d\n", mismatchCount-len(mismatches))
	}

	if err := writeOutput(outPath, builder.String()); err != nil {
		return err
	}
	if mismatchCount > 0 {
		if outPath != "" {
			return fmt.Errorf("found %d projection mismatch(es); details in %s", mismatchCount, outPath)
		}
		return fmt.Errorf("found %d projection mismatch(es)", mismatchCount)
	}
	return nil
}

// writeOverhead compares trigger-enabled and trigger-suppressed reward and
// adjustment writes. It runs against a disposable user and symbol so the
// benchmark can be repeated without touching seeded rows.
func writeOverhead(dbConfig config.Database, iterations, warmup int, outPath string) error {
	if iterations < 1 {
		return fmt.Errorf("iterations must be at least 1")
	}
	if warmup < 0 {
		return fmt.Errorf("warmup must not be negative")
	}

	db, err := open(dbConfig, dbConfig.DBName, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%10000000000)
	symbol := "WO" + suffix
	userID, err := createBenchmarkFixture(ctx, db, symbol, "wo_user_"+suffix)
	if err != nil {
		return err
	}
	defer cleanupBenchmarkFixture(db, userID, symbol)

	// A stable reward for the adjustment measurements.
	var adjustmentRewardID int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO rewards (user_id, stock_symbol, original_quantity, quantity, created_at)
		VALUES ($1, $2, 10, 10, ((market_today() - 4000) + time '09:15')::timestamp)
		RETURNING id
	`, userID, symbol).Scan(&adjustmentRewardID); err != nil {
		return fmt.Errorf("create adjustment fixture reward: %w", err)
	}

	type result struct {
		operation string
		mode      string
		samples   []time.Duration
	}

	var results []result

	rewardSequence := 0
	insertReward := func(tx *sql.Tx) error {
		day := 5000 - rewardSequence
		rewardSequence++
		var id int
		return tx.QueryRowContext(ctx, `
			INSERT INTO rewards (user_id, stock_symbol, original_quantity, quantity, created_at)
			VALUES ($1, $2, 10, 10, ((market_today() - $3::INTEGER) + time '09:15')::timestamp)
			RETURNING id
		`, userID, symbol, day).Scan(&id)
	}

	insertAdjustment := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO adjustments (
				reward_id, adjustment_type, delta_quantity, delta_amount, reason, effective_date
			)
			VALUES ($1, 'manual_correction', 0, 1, 'write-overhead', market_today())
		`, adjustmentRewardID)
		return err
	}

	for _, mode := range []struct {
		name string
		skip bool
	}{
		{name: "triggers_on", skip: false},
		{name: "triggers_off", skip: true},
	} {
		samples, err := measureWrites(ctx, db, iterations, warmup, mode.skip, insertReward)
		if err != nil {
			return fmt.Errorf("measure reward insert (%s): %w", mode.name, err)
		}
		results = append(results, result{
			operation: "reward_insert",
			mode:      mode.name,
			samples:   samples,
		})

		samples, err = measureWrites(ctx, db, iterations, warmup, mode.skip, insertAdjustment)
		if err != nil {
			return fmt.Errorf("measure adjustment insert (%s): %w", mode.name, err)
		}
		results = append(results, result{
			operation: "adjustment_insert",
			mode:      mode.name,
			samples:   samples,
		})
	}

	var builder strings.Builder
	builder.WriteString("operation\tmode\tsamples\tmean_ms\tp50_ms\tp95_ms\tp99_ms\tmax_ms\n")
	for _, item := range results {
		mean, p50, p95, p99, max := summarizeLatency(item.samples)
		fmt.Fprintf(
			&builder,
			"%s\t%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\n",
			item.operation,
			item.mode,
			len(item.samples),
			durationMillis(mean),
			durationMillis(p50),
			durationMillis(p95),
			durationMillis(p99),
			durationMillis(max),
		)
	}
	return writeOutput(outPath, builder.String())
}

func measureWrites(
	ctx context.Context,
	db *sql.DB,
	iterations, warmup int,
	skipTriggers bool,
	fn func(*sql.Tx) error,
) ([]time.Duration, error) {
	samples := make([]time.Duration, 0, iterations)
	for i := -warmup; i < iterations; i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}

		if skipTriggers {
			if _, err := tx.ExecContext(ctx, `SET LOCAL stocky.skip_projection_refresh = 'on'`); err != nil {
				_ = tx.Rollback()
				return nil, err
			}
		}

		started := time.Now()
		err = fn(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		elapsed := time.Since(started)
		if err != nil {
			return nil, err
		}
		if i >= 0 {
			samples = append(samples, elapsed)
		}
	}
	return samples, nil
}

func summarizeLatency(samples []time.Duration) (mean, p50, p95, p99, max time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0, 0, 0
	}

	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, sample := range sorted {
		total += sample
	}

	nearest := func(percent int) time.Duration {
		index := (len(sorted)*percent+99)/100 - 1
		if index < 0 {
			index = 0
		}
		if index >= len(sorted) {
			index = len(sorted) - 1
		}
		return sorted[index]
	}

	return total / time.Duration(len(sorted)), nearest(50), nearest(95), nearest(99), sorted[len(sorted)-1]
}

func durationMillis(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// rebuildCheck runs cmd/rebuild-projections --online while writers mutate a
// disposable user. It then verifies that the live writes survived and that the
// reconciliation baseline reports no drift. A second run records repeat cost
// and verifies the online path remains safe.
func rebuildCheck(
	dbConfig config.Database,
	binary string,
	runs, writers int,
	writeInterval time.Duration,
	writeLimit int,
	outPath string,
) error {
	if runs < 1 {
		return fmt.Errorf("rebuild-runs must be at least 1")
	}
	if writers < 1 {
		return fmt.Errorf("writers must be at least 1")
	}
	if writeInterval < 0 {
		return fmt.Errorf("write-interval must not be negative")
	}
	if writeLimit < 1 {
		return fmt.Errorf("write-limit must be at least 1")
	}

	if binary == "" {
		binary = filepath.Join("bench", "bin", "rebuild-projections")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
	}
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf(
			"rebuild binary %s not found; build it with `go build -o %s ./cmd/rebuild-projections`: %w",
			binary, binary, err,
		)
	}

	db, err := open(dbConfig, dbConfig.DBName, 4)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%10000000000)
	type benchmarkFixture struct {
		userID int
		symbol string
	}
	fixtures := make([]benchmarkFixture, 0, writers)
	for i := 0; i < writers; i++ {
		fixture := benchmarkFixture{
			symbol: fmt.Sprintf("RC%s%02d", suffix, i),
		}
		fixture.userID, err = createBenchmarkFixture(
			ctx,
			db,
			fixture.symbol,
			fmt.Sprintf("rc_user_%s_%d", suffix, i),
		)
		if err != nil {
			for _, created := range fixtures {
				cleanupBenchmarkFixture(db, created.userID, created.symbol)
			}
			return err
		}
		fixtures = append(fixtures, fixture)
	}
	defer func() {
		for _, fixture := range fixtures {
			cleanupBenchmarkFixture(db, fixture.userID, fixture.symbol)
		}
	}()

	userIDs := make([]int, 0, len(fixtures))
	for _, fixture := range fixtures {
		userIDs = append(userIDs, fixture.userID)
	}

	stop := make(chan struct{})
	writeErrors := make(chan error, writers)
	var writeCount int64
	var sequence int64
	var writerGroup sync.WaitGroup

	for i := range fixtures {
		fixture := fixtures[i]
		writerGroup.Add(1)
		go func() {
			defer writerGroup.Done()
			for inserted := 0; inserted < writeLimit; inserted++ {
				select {
				case <-stop:
					return
				default:
				}

				day := 10000 + int(atomic.AddInt64(&sequence, 1))
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					select {
					case writeErrors <- err:
					default:
					}
					return
				}

				var rewardID int
				err = tx.QueryRowContext(ctx, `
					INSERT INTO rewards (user_id, stock_symbol, original_quantity, quantity, created_at)
					VALUES ($1, $2, 7, 7, ((market_today() - $3::INTEGER) + time '09:15')::timestamp)
					RETURNING id
				`, fixture.userID, fixture.symbol, day).Scan(&rewardID)
				if err == nil {
					_, err = tx.ExecContext(ctx, `
						INSERT INTO adjustments (
							reward_id, adjustment_type, delta_quantity, delta_amount, reason, effective_date
						)
						VALUES ($1, 'manual_correction', 0, 2, 'rebuild-check', market_today())
					`, rewardID)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
				if err != nil {
					select {
					case writeErrors <- err:
					default:
					}
					return
				}

				atomic.AddInt64(&writeCount, 1)
				select {
				case <-stop:
					return
				case <-time.After(writeInterval):
				}
			}
		}()
	}

	type rebuildRun struct {
		seconds     float64
		afterWrites int64
		mismatches  int
		rewards     int
		states      int
	}

	rebuildRuns := make([]rebuildRun, 0, runs)
	for run := 0; run < runs; run++ {
		started := time.Now()
		if err := runOnlineRebuild(ctx, binary, dbConfig); err != nil {
			close(stop)
			writerGroup.Wait()
			return fmt.Errorf("online rebuild run %d: %w", run+1, err)
		}
		elapsed := time.Since(started)

		var mismatches, rewards, states int
		// One statement keeps the reward/reward_state counts in the same
		// snapshot while the writers are still committing.
		if err := db.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM rewards WHERE user_id = ANY($1::INTEGER[])),
				(SELECT COUNT(*) FROM reward_state WHERE user_id = ANY($1::INTEGER[])),
				(SELECT COUNT(*) FROM projection_mismatches_for_users($1::INTEGER[]))
		`, pq.Array(userIDs)).Scan(&rewards, &states, &mismatches); err != nil {
			close(stop)
			writerGroup.Wait()
			return fmt.Errorf("verify fixture projections after run %d: %w", run+1, err)
		}

		rebuildRuns = append(rebuildRuns, rebuildRun{
			seconds:     elapsed.Seconds(),
			afterWrites: atomic.LoadInt64(&writeCount),
			mismatches:  mismatches,
			rewards:     rewards,
			states:      states,
		})
	}

	close(stop)
	writerGroup.Wait()
	close(writeErrors)

	if err := firstError(writeErrors); err != nil {
		return fmt.Errorf("concurrent writer failed: %w", err)
	}

	var finalMismatches int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM projection_mismatches_for_users($1::INTEGER[])
	`, pq.Array(userIDs)).Scan(&finalMismatches); err != nil {
		return fmt.Errorf("verify quiesced fixture projections: %w", err)
	}
	if finalMismatches != 0 {
		return fmt.Errorf("quiesced fixture has %d reconciliation mismatch(es)", finalMismatches)
	}

	var builder strings.Builder
	builder.WriteString("run\tseconds\tconcurrent_writes\tfixture_rewards\tfixture_reward_state\tmismatches\n")
	for index, run := range rebuildRuns {
		fmt.Fprintf(
			&builder,
			"%d\t%.3f\t%d\t%d\t%d\t%d\n",
			index+1,
			run.seconds,
			run.afterWrites,
			run.rewards,
			run.states,
			run.mismatches,
		)
		if run.mismatches != 0 {
			return fmt.Errorf(
				"online rebuild run %d left %d reconciliation mismatch(es) for the live fixture",
				index+1, run.mismatches,
			)
		}
		if run.rewards != run.states {
			return fmt.Errorf(
				"online rebuild run %d: fixture has %d rewards but %d reward_state rows",
				index+1, run.rewards, run.states,
			)
		}
	}
	fmt.Fprintf(&builder, "concurrent_writes\t%d\n", atomic.LoadInt64(&writeCount))
	fmt.Fprintf(&builder, "quiesced_mismatches\t%d\n", finalMismatches)
	return writeOutput(outPath, builder.String())
}

func runOnlineRebuild(ctx context.Context, binary string, dbConfig config.Database) error {
	cmd := exec.CommandContext(
		ctx,
		binary,
		"--online",
		"--skip-vacuum",
		"--database-url",
		connectionURL(dbConfig, dbConfig.DBName),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func firstError(errors <-chan error) error {
	for err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func createBenchmarkFixture(ctx context.Context, db *sql.DB, symbol, username string) (int, error) {
	var userID int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO users (username, email) VALUES ($1, $2) RETURNING id
	`, username, username+"@bench.local").Scan(&userID); err != nil {
		return 0, fmt.Errorf("create benchmark user: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO stock_prices (stock_symbol, price, source, fetched_at)
		VALUES ($1, 100.0000, 'simulator', NOW())
	`, symbol); err != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		return 0, fmt.Errorf("create benchmark price: %w", err)
	}

	return userID, nil
}

func cleanupBenchmarkFixture(db *sql.DB, userID int, symbol string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tx, err := db.BeginTx(cleanupCtx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: clean up benchmark fixture: %v\n", err)
		return
	}
	defer tx.Rollback()

	// A live-trigger cleanup would refresh the user once per deleted reward and
	// turn a large fixture into quadratic work.
	if _, err := tx.ExecContext(cleanupCtx, `SET LOCAL stocky.skip_projection_refresh = 'on'`); err != nil {
		fmt.Fprintf(os.Stderr, "warning: suppress projection refresh during cleanup: %v\n", err)
		return
	}

	statements := []struct {
		label string
		query string
		args  []any
	}{
		{label: "adjustments", query: `
			DELETE FROM adjustments
			WHERE reward_id IN (SELECT id FROM rewards WHERE user_id = $1)
		`, args: []any{userID}},
		{label: "rewards", query: `DELETE FROM rewards WHERE user_id = $1`, args: []any{userID}},
		{label: "user", query: `DELETE FROM users WHERE id = $1`, args: []any{userID}},
		{label: "price", query: `DELETE FROM stock_prices WHERE stock_symbol = $1`, args: []any{symbol}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(cleanupCtx, statement.query, statement.args...); err != nil {
			fmt.Fprintf(os.Stderr, "warning: clean up benchmark %s: %v\n", statement.label, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: commit benchmark cleanup: %v\n", err)
	}
}

func connectionURL(dbConfig config.Database, databaseName string) string {
	connectionURL := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(dbConfig.User, dbConfig.Password),
		Host:   net.JoinHostPort(dbConfig.Host, dbConfig.DbPort),
		Path:   databaseName,
	}
	query := connectionURL.Query()
	query.Set("sslmode", dbConfig.SSLMode)
	connectionURL.RawQuery = query.Encode()
	return connectionURL.String()
}

func open(dbConfig config.Database, databaseName string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("postgres", connectionURL(dbConfig, databaseName))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %w", databaseName, err)
	}
	return db, nil
}

func stripPsqlMeta(text string) string {
	lines := strings.Split(text, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "\\") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func splitStatements(text string) []string {
	parts := strings.Split(text, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		statements = append(statements, part)
	}
	return statements
}

func writeOutput(path, content string) error {
	if path == "" {
		fmt.Print(content)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
