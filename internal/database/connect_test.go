package database

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDSNIncludesPoolTimeouts(t *testing.T) {
	dsn := BuildDSN(ConnectOptions{
		Host:                     "localhost",
		Port:                     "5432",
		User:                     "stocky",
		Password:                 "secret",
		DBName:                   "stocky_db",
		SSLMode:                  "disable",
		StatementTimeout:         3 * time.Second,
		IdleInTransactionTimeout: 10 * time.Second,
		ApplicationName:          "stocky-api",
	})

	for _, want := range []string{
		"host=localhost",
		"port=5432",
		"user=stocky",
		"dbname=stocky_db",
		"sslmode=disable",
		"application_name=stocky-api",
		"statement_timeout=3000",
		"idle_in_transaction_session_timeout=10000",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("BuildDSN() = %q, missing %q", dsn, want)
		}
	}
}

func TestBuildDSNOmitsZeroOptionalSettings(t *testing.T) {
	dsn := BuildDSN(ConnectOptions{
		Host:     "localhost",
		Port:     "5432",
		User:     "stocky",
		Password: "secret",
		DBName:   "stocky_db",
		SSLMode:  "disable",
	})

	if strings.Contains(dsn, "statement_timeout") {
		t.Errorf("BuildDSN() = %q, want no statement_timeout", dsn)
	}
	if strings.Contains(dsn, "idle_in_transaction_session_timeout") {
		t.Errorf("BuildDSN() = %q, want no idle-in-transaction timeout", dsn)
	}
	if strings.Contains(dsn, "application_name") {
		t.Errorf("BuildDSN() = %q, want no application_name", dsn)
	}
}

func TestOpenDSNRejectsEmptyDSN(t *testing.T) {
	if _, err := OpenDSN("   ", PoolOptions{}); err == nil {
		t.Fatal("OpenDSN() accepted an empty DSN")
	}
}

func TestApplyDSNSettingsToURL(t *testing.T) {
	dsn, err := applyDSNSettings("postgres://user:pass@replica:5432/stocky?sslmode=disable", PoolOptions{
		StatementTimeout:         3 * time.Second,
		IdleInTransactionTimeout: 10 * time.Second,
		ApplicationName:          "stocky-read",
	})
	if err != nil {
		t.Fatalf("applyDSNSettings() error = %v", err)
	}

	for _, want := range []string{
		"application_name=stocky-read",
		"statement_timeout=3000",
		"idle_in_transaction_session_timeout=10000",
		"sslmode=disable",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("applyDSNSettings() = %q, missing %q", dsn, want)
		}
	}
}

func TestApplyDSNSettingsToKeywordDSN(t *testing.T) {
	dsn, err := applyDSNSettings("host=replica dbname=stocky", PoolOptions{
		StatementTimeout:         3 * time.Second,
		IdleInTransactionTimeout: 10 * time.Second,
		ApplicationName:          "stocky-read",
	})
	if err != nil {
		t.Fatalf("applyDSNSettings() error = %v", err)
	}

	for _, want := range []string{
		"host=replica",
		"dbname=stocky",
		"application_name=stocky-read",
		"statement_timeout=3000",
		"idle_in_transaction_session_timeout=10000",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("applyDSNSettings() = %q, missing %q", dsn, want)
		}
	}
}
