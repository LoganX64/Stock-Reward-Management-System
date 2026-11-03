package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// ConnectOptions describes a PostgreSQL pool. StatementTimeout and
// IdleInTransactionTimeout are sent as PostgreSQL run-time parameters in the
// connection string, so every connection in the pool inherits them.
type ConnectOptions struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	SSLMode  string

	MaxOpenConns             int
	MaxIdleConns             int
	ConnMaxLifetime          time.Duration
	ConnMaxIdleTime          time.Duration
	StatementTimeout         time.Duration
	IdleInTransactionTimeout time.Duration

	ApplicationName string
}

// PoolOptions describes the pool settings used when an existing DSN is opened.
type PoolOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration

	// Optional run-time parameters applied to the DSN before opening. This lets
	// an optional read replica inherit the same statement timeout as the API
	// pool even though its credentials come from a separate DSN.
	StatementTimeout         time.Duration
	IdleInTransactionTimeout time.Duration
	ApplicationName          string
}

// BuildDSN returns a lib/pq keyword connection string with the pool's
// statement timeouts included. Durations are sent as milliseconds because
// PostgreSQL accepts integer GUC values without quoting.
func BuildDSN(opts ConnectOptions) string {
	parts := []string{
		"host=" + opts.Host,
		"port=" + opts.Port,
		"user=" + opts.User,
		"password=" + opts.Password,
		"dbname=" + opts.DBName,
		"sslmode=" + opts.SSLMode,
	}

	if opts.ApplicationName != "" {
		parts = append(parts, "application_name="+opts.ApplicationName)
	}
	if opts.StatementTimeout > 0 {
		parts = append(parts, fmt.Sprintf("statement_timeout=%d", opts.StatementTimeout.Milliseconds()))
	}
	if opts.IdleInTransactionTimeout > 0 {
		parts = append(parts, fmt.Sprintf("idle_in_transaction_session_timeout=%d", opts.IdleInTransactionTimeout.Milliseconds()))
	}

	return strings.Join(parts, " ")
}

// Open opens and verifies a PostgreSQL pool.
func Open(opts ConnectOptions) (*sql.DB, error) {
	db, err := sql.Open("postgres", BuildDSN(opts))
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	configurePool(db, PoolOptions{
		MaxOpenConns:    opts.MaxOpenConns,
		MaxIdleConns:    opts.MaxIdleConns,
		ConnMaxLifetime: opts.ConnMaxLifetime,
		ConnMaxIdleTime: opts.ConnMaxIdleTime,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	return db, nil
}

// OpenDSN opens an existing connection string, used for an optional read
// replica whose credentials may differ from the primary.
func OpenDSN(dsn string, opts PoolOptions) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("empty database DSN")
	}

	dsn, err := applyDSNSettings(dsn, opts)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	configurePool(db, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	return db, nil
}

// applyDSNSettings adds run-time parameters to a URL or keyword DSN. Existing
// values are replaced so the configured API timeout always wins.
func applyDSNSettings(dsn string, opts PoolOptions) (string, error) {
	trimmed := strings.TrimSpace(dsn)

	if strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("parse database DSN: %w", err)
		}
		query := parsed.Query()
		if opts.ApplicationName != "" {
			query.Set("application_name", opts.ApplicationName)
		}
		if opts.StatementTimeout > 0 {
			query.Set("statement_timeout", strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10))
		}
		if opts.IdleInTransactionTimeout > 0 {
			query.Set("idle_in_transaction_session_timeout", strconv.FormatInt(opts.IdleInTransactionTimeout.Milliseconds(), 10))
		}
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}

	parts := []string{trimmed}
	if opts.ApplicationName != "" {
		parts = append(parts, "application_name="+opts.ApplicationName)
	}
	if opts.StatementTimeout > 0 {
		parts = append(parts, fmt.Sprintf("statement_timeout=%d", opts.StatementTimeout.Milliseconds()))
	}
	if opts.IdleInTransactionTimeout > 0 {
		parts = append(parts, fmt.Sprintf("idle_in_transaction_session_timeout=%d", opts.IdleInTransactionTimeout.Milliseconds()))
	}
	return strings.Join(parts, " "), nil
}

func configurePool(db *sql.DB, opts PoolOptions) {
	if opts.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConns)
	}
	if opts.MaxIdleConns >= 0 {
		db.SetMaxIdleConns(opts.MaxIdleConns)
	}
	if opts.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	if opts.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
	}
}
