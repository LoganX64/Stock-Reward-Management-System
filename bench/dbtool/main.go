// Command dbtool performs the database-side work for the Phase 0 benchmark.
//
// It uses the same configuration loader as the API, so it never needs the
// password to be copied into a shell environment or printed. It only reports
// connection success/failure and never logs credentials.
package main

import (
	"context"
	"database/sql"
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
	service := flag.String("service", "postgresql-x64-17", "PostgreSQL Windows service name for enable-stats")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		fmt.Fprintln(os.Stderr, "usage: dbtool <setup|seed|explain|stats|settings|activity|sessions|stop-sessions|enable-stats|disable-stats|reset-stats|counts> [flags]")
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
		if err := setup(cfg.Database); err != nil {
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
	default:
		fatal("unknown command %q", command)
	}
}

func setup(dbConfig config.Database) error {
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
	if errors.Is(err, sql.ErrNoRows) || exists != 1 {
		statement := fmt.Sprintf(`CREATE DATABASE "%s"`, dbConfig.DBName)
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
	fmt.Fprintf(&builder, "Stocky Phase 0 query plans\nDatabase: %s\n", dbConfig.DBName)

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

func open(dbConfig config.Database, databaseName string, maxConns int) (*sql.DB, error) {
	connectionURL := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(dbConfig.User, dbConfig.Password),
		Host:   net.JoinHostPort(dbConfig.Host, dbConfig.DbPort),
		Path:   databaseName,
	}
	query := connectionURL.Query()
	query.Set("sslmode", dbConfig.SSLMode)
	connectionURL.RawQuery = query.Encode()

	db, err := sql.Open("postgres", connectionURL.String())
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
