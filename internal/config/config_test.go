package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envValues(values map[string]string) func(string, string) string {
	return func(key, defaultValue string) string {
		if value, ok := values[key]; ok {
			return value
		}
		return defaultValue
	}
}

func TestLoadProjectionDefaults(t *testing.T) {
	p, err := loadProjection(envValues(nil))
	if err != nil {
		t.Fatalf("loadProjection() error = %v", err)
	}

	if p.RefreshInterval != 30*time.Second {
		t.Errorf("RefreshInterval = %s, want 30s", p.RefreshInterval)
	}
	if p.RefreshBatch != 200 {
		t.Errorf("RefreshBatch = %d, want 200", p.RefreshBatch)
	}
	if p.QueueMaxAttempts != 5 {
		t.Errorf("QueueMaxAttempts = %d, want 5", p.QueueMaxAttempts)
	}
	if p.QueueRetention != 168*time.Hour {
		t.Errorf("QueueRetention = %s, want 168h", p.QueueRetention)
	}
	if p.QueueFailedRetention != 720*time.Hour {
		t.Errorf("QueueFailedRetention = %s, want 720h", p.QueueFailedRetention)
	}
	if p.QueueAlertDepth != 1000 {
		t.Errorf("QueueAlertDepth = %d, want 1000", p.QueueAlertDepth)
	}
	if p.QueueAlertLag != 15*time.Minute {
		t.Errorf("QueueAlertLag = %s, want 15m", p.QueueAlertLag)
	}
	if p.ReconciliationInterval != time.Hour {
		t.Errorf("ReconciliationInterval = %s, want 1h", p.ReconciliationInterval)
	}
	if p.ReconciliationSamplePercent != 0.1 {
		t.Errorf("ReconciliationSamplePercent = %v, want 0.1", p.ReconciliationSamplePercent)
	}
	if p.ReconciliationMaxUsers != 500 {
		t.Errorf("ReconciliationMaxUsers = %d, want 500", p.ReconciliationMaxUsers)
	}
	if p.SyncUserLimit != 50 {
		t.Errorf("SyncUserLimit = %d, want 50", p.SyncUserLimit)
	}
	if p.CorporateActionInterval != 15*time.Minute {
		t.Errorf("CorporateActionInterval = %s, want 15m", p.CorporateActionInterval)
	}
}

func TestLoadProjectionOverrides(t *testing.T) {
	p, err := loadProjection(envValues(map[string]string{
		"PROJECTION_REFRESH_INTERVAL":              "45s",
		"PROJECTION_REFRESH_BATCH":                 "25",
		"PROJECTION_QUEUE_MAX_ATTEMPTS":            "9",
		"PROJECTION_QUEUE_RETENTION":               "24h",
		"PROJECTION_QUEUE_FAILED_RETENTION":        "48h",
		"PROJECTION_QUEUE_ALERT_DEPTH":             "10",
		"PROJECTION_QUEUE_ALERT_LAG":               "2m",
		"PROJECTION_RECONCILIATION_INTERVAL":       "10m",
		"PROJECTION_RECONCILIATION_SAMPLE_PERCENT": "2.5",
		"PROJECTION_RECONCILIATION_MAX_USERS":      "40",
		"PROJECTION_SYNC_USER_LIMIT":               "0",
		"CORPORATE_ACTION_INTERVAL":                "5m",
	}))
	if err != nil {
		t.Fatalf("loadProjection() error = %v", err)
	}

	if p.RefreshInterval != 45*time.Second || p.RefreshBatch != 25 {
		t.Errorf("refresh overrides = %s/%d, want 45s/25", p.RefreshInterval, p.RefreshBatch)
	}
	if p.QueueMaxAttempts != 9 || p.QueueAlertDepth != 10 || p.QueueAlertLag != 2*time.Minute {
		t.Errorf("queue overrides = %d/%d/%s", p.QueueMaxAttempts, p.QueueAlertDepth, p.QueueAlertLag)
	}
	if p.ReconciliationSamplePercent != 2.5 || p.ReconciliationMaxUsers != 40 {
		t.Errorf("reconciliation overrides = %v/%d", p.ReconciliationSamplePercent, p.ReconciliationMaxUsers)
	}
	if p.SyncUserLimit != 0 {
		t.Errorf("SyncUserLimit = %d, want 0", p.SyncUserLimit)
	}
}

func TestLoadProjectionRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"refresh interval":    {"PROJECTION_REFRESH_INTERVAL": "0s"},
		"refresh batch":       {"PROJECTION_REFRESH_BATCH": "0"},
		"max attempts":        {"PROJECTION_QUEUE_MAX_ATTEMPTS": "0"},
		"queue retention":     {"PROJECTION_QUEUE_RETENTION": "-1h"},
		"alert depth":         {"PROJECTION_QUEUE_ALERT_DEPTH": "0"},
		"sample percent":      {"PROJECTION_RECONCILIATION_SAMPLE_PERCENT": "101"},
		"max users":           {"PROJECTION_RECONCILIATION_MAX_USERS": "0"},
		"negative sync limit": {"PROJECTION_SYNC_USER_LIMIT": "-1"},
		"corporate action":    {"CORPORATE_ACTION_INTERVAL": "0s"},
		"invalid float":       {"PROJECTION_RECONCILIATION_SAMPLE_PERCENT": "many"},
	}

	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadProjection(envValues(values)); err == nil {
				t.Fatal("loadProjection() error = nil, want validation error")
			}
		})
	}
}

func TestLoadHTTPServerDefaults(t *testing.T) {
	h, err := loadHTTPServer(envValues(nil))
	if err != nil {
		t.Fatalf("loadHTTPServer() error = %v", err)
	}
	if h.Port != ":8080" {
		t.Errorf("Port = %q, want :8080", h.Port)
	}
	if h.RequestTimeout != 10*time.Second {
		t.Errorf("RequestTimeout = %s, want 10s", h.RequestTimeout)
	}
	if !h.MetricsEnabled {
		t.Error("MetricsEnabled = false, want true")
	}
}

func TestLoadHTTPServerOverrides(t *testing.T) {
	h, err := loadHTTPServer(envValues(map[string]string{
		"HTTP_PORT":            "9090",
		"HTTP_REQUEST_TIMEOUT": "5s",
		"METRICS_ENABLED":      "false",
	}))
	if err != nil {
		t.Fatalf("loadHTTPServer() error = %v", err)
	}
	if h.Port != ":9090" || h.RequestTimeout != 5*time.Second || h.MetricsEnabled {
		t.Errorf("loadHTTPServer() = %+v, want :9090/5s/false", h)
	}
}

func TestLoadHTTPServerRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"request timeout": {"HTTP_REQUEST_TIMEOUT": "0s"},
		"bad duration":    {"HTTP_REQUEST_TIMEOUT": "soon"},
		"bad bool":        {"METRICS_ENABLED": "maybe"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadHTTPServer(envValues(values)); err == nil {
				t.Fatal("loadHTTPServer() error = nil, want validation error")
			}
		})
	}
}

func TestLoadDatabaseDefaults(t *testing.T) {
	d, err := loadDatabase(envValues(nil))
	if err != nil {
		t.Fatalf("loadDatabase() error = %v", err)
	}

	if d.Host != "localhost" || d.DbPort != "5432" || d.DBName != "assignment" {
		t.Errorf("database defaults = %+v", d)
	}
	if d.MaxOpenConns != 25 || d.MaxIdleConns != 10 {
		t.Errorf("API pool = %d/%d, want 25/10", d.MaxOpenConns, d.MaxIdleConns)
	}
	if d.ConnMaxLifetime != 30*time.Minute || d.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("connection lifetimes = %s/%s", d.ConnMaxLifetime, d.ConnMaxIdleTime)
	}
	if d.StatementTimeout != 3*time.Second || d.IdleInTransactionTimeout != 10*time.Second {
		t.Errorf("API timeouts = %s/%s", d.StatementTimeout, d.IdleInTransactionTimeout)
	}
	if d.JobMaxOpenConns != 5 || d.JobMaxIdleConns != 2 || d.JobStatementTimeout != 5*time.Minute {
		t.Errorf("job pool = %d/%d/%s", d.JobMaxOpenConns, d.JobMaxIdleConns, d.JobStatementTimeout)
	}
	if d.ReadDSN != "" || d.ReadMaxOpenConns != 25 || d.ReadMaxIdleConns != 10 {
		t.Errorf("read replica defaults = %q/%d/%d", d.ReadDSN, d.ReadMaxOpenConns, d.ReadMaxIdleConns)
	}
}

func TestLoadDatabaseOverrides(t *testing.T) {
	d, err := loadDatabase(envValues(map[string]string{
		"DB_HOST":                        "db.internal",
		"DB_PORT":                        "5433",
		"POSTGRES_USER":                  "stocky",
		"POSTGRES_PASSWORD":              "secret",
		"POSTGRES_DB":                    "stocky_db",
		"DB_SSLMODE":                     "require",
		"DB_MAX_OPEN_CONNS":              "50",
		"DB_MAX_IDLE_CONNS":              "25",
		"DB_CONN_MAX_LIFETIME":           "15m",
		"DB_CONN_MAX_IDLE_TIME":          "2m",
		"DB_STATEMENT_TIMEOUT":           "5s",
		"DB_IDLE_IN_TRANSACTION_TIMEOUT": "30s",
		"DB_JOB_MAX_OPEN_CONNS":          "8",
		"DB_JOB_MAX_IDLE_CONNS":          "4",
		"DB_JOB_STATEMENT_TIMEOUT":       "10m",
		"DB_READ_DSN":                    "host=replica dbname=stocky_db",
		"DB_READ_MAX_OPEN_CONNS":         "40",
		"DB_READ_MAX_IDLE_CONNS":         "20",
	}))
	if err != nil {
		t.Fatalf("loadDatabase() error = %v", err)
	}

	if d.Host != "db.internal" || d.DbPort != "5433" || d.User != "stocky" || d.DBName != "stocky_db" || d.SSLMode != "require" {
		t.Errorf("connection overrides = %+v", d)
	}
	if d.MaxOpenConns != 50 || d.MaxIdleConns != 25 || d.ConnMaxLifetime != 15*time.Minute || d.ConnMaxIdleTime != 2*time.Minute {
		t.Errorf("pool overrides = %+v", d)
	}
	if d.StatementTimeout != 5*time.Second || d.IdleInTransactionTimeout != 30*time.Second {
		t.Errorf("API timeouts = %s/%s", d.StatementTimeout, d.IdleInTransactionTimeout)
	}
	if d.JobMaxOpenConns != 8 || d.JobMaxIdleConns != 4 || d.JobStatementTimeout != 10*time.Minute {
		t.Errorf("job overrides = %+v", d)
	}
	if d.ReadDSN != "host=replica dbname=stocky_db" || d.ReadMaxOpenConns != 40 || d.ReadMaxIdleConns != 20 {
		t.Errorf("read overrides = %+v", d)
	}
}

func TestLoadDatabaseRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"max open":              {"DB_MAX_OPEN_CONNS": "0"},
		"idle above open":       {"DB_MAX_OPEN_CONNS": "5", "DB_MAX_IDLE_CONNS": "6"},
		"negative idle":         {"DB_MAX_IDLE_CONNS": "-1"},
		"conn lifetime":         {"DB_CONN_MAX_LIFETIME": "0s"},
		"conn idle":             {"DB_CONN_MAX_IDLE_TIME": "0s"},
		"statement timeout":     {"DB_STATEMENT_TIMEOUT": "0s"},
		"idle transaction":      {"DB_IDLE_IN_TRANSACTION_TIMEOUT": "-1s"},
		"job max open":          {"DB_JOB_MAX_OPEN_CONNS": "0"},
		"job idle above open":   {"DB_JOB_MAX_OPEN_CONNS": "2", "DB_JOB_MAX_IDLE_CONNS": "3"},
		"job statement timeout": {"DB_JOB_STATEMENT_TIMEOUT": "0s"},
		"read max open":         {"DB_READ_MAX_OPEN_CONNS": "0"},
		"read idle above open":  {"DB_READ_MAX_OPEN_CONNS": "2", "DB_READ_MAX_IDLE_CONNS": "3"},
		"invalid integer":       {"DB_MAX_OPEN_CONNS": "many"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadDatabase(envValues(values)); err == nil {
				t.Fatal("loadDatabase() error = nil, want validation error")
			}
		})
	}
}

func TestLoadMarketDataDefaults(t *testing.T) {
	md, err := loadMarketData(envValues(nil))
	if err != nil {
		t.Fatalf("loadMarketData() error = %v", err)
	}

	if !md.Enabled {
		t.Error("Enabled = false, want true")
	}
	if md.Provider != "yahoo" {
		t.Errorf("Provider = %q, want yahoo", md.Provider)
	}
	if md.Exchange != "NSE" {
		t.Errorf("Exchange = %q, want NSE", md.Exchange)
	}
	if md.SymbolSuffix != ".NS" {
		t.Errorf("SymbolSuffix = %q, want .NS", md.SymbolSuffix)
	}
	if md.TickInterval != 60*time.Minute {
		t.Errorf("TickInterval = %s, want 60m", md.TickInterval)
	}
	if md.MaxStale != 90*time.Minute {
		t.Errorf("MaxStale = %s, want 90m", md.MaxStale)
	}
	if md.CloseGrace != 20*time.Minute {
		t.Errorf("CloseGrace = %s, want 20m", md.CloseGrace)
	}
	if !md.MarketHoursOnly {
		t.Error("MarketHoursOnly = false, want true")
	}
	if md.FinalQuoteDelay != 5*time.Minute {
		t.Errorf("FinalQuoteDelay = %s, want 5m", md.FinalQuoteDelay)
	}
	if md.HTTPTimeout != 10*time.Second {
		t.Errorf("HTTPTimeout = %s, want 10s", md.HTTPTimeout)
	}
	if md.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", md.MaxRetries)
	}
	if md.RetryBase != time.Second {
		t.Errorf("RetryBase = %s, want 1s", md.RetryBase)
	}
	if md.RateLimit != time.Second {
		t.Errorf("RateLimit = %s, want 1s", md.RateLimit)
	}
	if md.UserAgent == "" {
		t.Error("UserAgent is empty")
	}
	if md.YahooBaseURL != "https://query1.finance.yahoo.com" {
		t.Errorf("YahooBaseURL = %q", md.YahooBaseURL)
	}
	if !md.BhavcopyEnabled {
		t.Error("BhavcopyEnabled = false, want true")
	}
	if md.BhavcopyBaseURL != "https://nsearchives.nseindia.com" {
		t.Errorf("BhavcopyBaseURL = %q", md.BhavcopyBaseURL)
	}
	if md.BhavcopyTime != "18:15" {
		t.Errorf("BhavcopyTime = %q, want 18:15", md.BhavcopyTime)
	}
	if md.BhavcopyRetryUntil != "20:30" {
		t.Errorf("BhavcopyRetryUntil = %q, want 20:30", md.BhavcopyRetryUntil)
	}
	if md.BhavcopyRetryInterval != 30*time.Minute {
		t.Errorf("BhavcopyRetryInterval = %s, want 30m", md.BhavcopyRetryInterval)
	}
	if len(md.Holidays) != 0 {
		t.Errorf("Holidays = %v, want empty", md.Holidays)
	}
}

func TestLoadMarketDataValidOverrides(t *testing.T) {
	md, err := loadMarketData(envValues(map[string]string{
		"MARKET_DATA_ENABLED":           "false",
		"MARKET_DATA_PROVIDER":          " Simulator ",
		"MARKET_DATA_EXCHANGE":          "bse",
		"MARKET_DATA_SYMBOL_SUFFIX":     ".NSX",
		"MARKET_DATA_BHAVCOPY_ENABLED":  "false",
		"MARKET_DATA_MAX_RETRIES":       "2",
		"MARKET_DATA_TICK_INTERVAL":     "15m",
		"MARKET_DATA_USER_AGENT":        "custom-agent",
		"MARKET_DATA_YAHOO_BASE_URL":    "https://example.test/yahoo/",
		"MARKET_DATA_BHAVCOPY_BASE_URL": "http://example.test/nse/",
		"MARKET_HOLIDAYS":               "2026-01-26, 2026-11-02,2026-01-26",
	}))
	if err != nil {
		t.Fatalf("loadMarketData() error = %v", err)
	}

	if md.Enabled {
		t.Error("Enabled = true, want false")
	}
	if md.Provider != "simulator" {
		t.Errorf("Provider = %q, want simulator", md.Provider)
	}
	if md.Exchange != "BSE" {
		t.Errorf("Exchange = %q, want BSE", md.Exchange)
	}
	if md.SymbolSuffix != ".NSX" {
		t.Errorf("SymbolSuffix = %q, want .NSX", md.SymbolSuffix)
	}
	if md.BhavcopyEnabled {
		t.Error("BhavcopyEnabled = true, want false")
	}
	if md.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", md.MaxRetries)
	}
	if md.TickInterval != 15*time.Minute {
		t.Errorf("TickInterval = %s, want 15m", md.TickInterval)
	}
	if md.UserAgent != "custom-agent" {
		t.Errorf("UserAgent = %q, want custom-agent", md.UserAgent)
	}
	if md.YahooBaseURL != "https://example.test/yahoo" {
		t.Errorf("YahooBaseURL = %q", md.YahooBaseURL)
	}
	if md.BhavcopyBaseURL != "http://example.test/nse" {
		t.Errorf("BhavcopyBaseURL = %q", md.BhavcopyBaseURL)
	}
	if len(md.Holidays) != 2 || md.Holidays[0] != "2026-01-26" || md.Holidays[1] != "2026-11-02" {
		t.Errorf("Holidays = %v, want [2026-01-26 2026-11-02]", md.Holidays)
	}
}

func TestLoadMarketDataBhavcopyRequiresNSE(t *testing.T) {
	_, err := loadMarketData(envValues(map[string]string{
		"MARKET_DATA_EXCHANGE":         "BSE",
		"MARKET_DATA_BHAVCOPY_ENABLED": "true",
	}))
	if err == nil {
		t.Fatal("loadMarketData() accepted bhavcopy for BSE")
	}
	if !strings.Contains(err.Error(), "bhavcopy") {
		t.Errorf("error = %q, want bhavcopy message", err)
	}
}

func TestLoadMarketDataInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "provider", key: "MARKET_DATA_PROVIDER", value: "polygon", want: "MARKET_DATA_PROVIDER"},
		{name: "exchange", key: "MARKET_DATA_EXCHANGE", value: "NYSE", want: "MARKET_DATA_EXCHANGE"},
		{name: "tick duration", key: "MARKET_DATA_TICK_INTERVAL", value: "soon", want: "MARKET_DATA_TICK_INTERVAL"},
		{name: "zero max stale", key: "MARKET_DATA_MAX_STALE", value: "0s", want: "MARKET_DATA_MAX_STALE"},
		{name: "bad bool", key: "MARKET_DATA_ENABLED", value: "maybe", want: "MARKET_DATA_ENABLED"},
		{name: "bad integer", key: "MARKET_DATA_MAX_RETRIES", value: "many", want: "MARKET_DATA_MAX_RETRIES"},
		{name: "zero retries", key: "MARKET_DATA_MAX_RETRIES", value: "0", want: "MARKET_DATA_MAX_RETRIES"},
		{name: "bad bhavcopy time", key: "MARKET_DATA_BHAVCOPY_TIME", value: "25:00", want: "MARKET_DATA_BHAVCOPY_TIME"},
		{name: "bad retry window", key: "MARKET_DATA_BHAVCOPY_RETRY_UNTIL", value: "17:00", want: "MARKET_DATA_BHAVCOPY_RETRY_UNTIL"},
		{name: "bad holiday", key: "MARKET_HOLIDAYS", value: "2026-13-01", want: "MARKET_HOLIDAYS"},
		{name: "bad url", key: "MARKET_DATA_YAHOO_BASE_URL", value: "not-a-url", want: "MARKET_DATA_YAHOO_BASE_URL"},
		{name: "bad suffix", key: "MARKET_DATA_SYMBOL_SUFFIX", value: "NS", want: "MARKET_DATA_SYMBOL_SUFFIX"},
		{name: "negative rate limit", key: "MARKET_DATA_RATE_LIMIT", value: "-1s", want: "MARKET_DATA_RATE_LIMIT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadMarketData(envValues(map[string]string{tt.key: tt.value}))
			if err == nil {
				t.Fatalf("loadMarketData() accepted %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %s", err, tt.want)
			}
		})
	}
}

func TestParseHolidays(t *testing.T) {
	holidays, err := parseHolidays("2026-01-26, 2026-10-02,2026-01-26")
	if err != nil {
		t.Fatalf("parseHolidays() error = %v", err)
	}
	if len(holidays) != 2 || holidays[0] != "2026-01-26" || holidays[1] != "2026-10-02" {
		t.Errorf("parseHolidays() = %v, want [2026-01-26 2026-10-02]", holidays)
	}

	if holidays, err := parseHolidays(""); err != nil || len(holidays) != 0 {
		t.Errorf("parseHolidays(empty) = (%v, %v), want empty nil error", holidays, err)
	}
	if _, err := parseHolidays("2026-02-30"); err == nil {
		t.Error("parseHolidays() accepted an invalid date")
	}
	if _, err := parseHolidays("26-01-2026"); err == nil {
		t.Error("parseHolidays() accepted the wrong date format")
	}
}

func TestParseClock(t *testing.T) {
	tests := []struct {
		value string
		want  int
		ok    bool
	}{
		{value: "00:00", want: 0, ok: true},
		{value: "18:15", want: 18*60 + 15, ok: true},
		{value: "23:59", want: 23*60 + 59, ok: true},
		{value: "24:00", ok: false},
		{value: "18:60", ok: false},
		{value: "8:15", ok: false},
		{value: "", ok: false},
	}

	for _, tt := range tests {
		got, err := parseClock("TEST_CLOCK", tt.value)
		if tt.ok {
			if err != nil || got != tt.want {
				t.Errorf("parseClock(%q) = (%d, %v), want (%d, nil)", tt.value, got, err, tt.want)
			}
		} else if err == nil {
			t.Errorf("parseClock(%q) accepted an invalid time", tt.value)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, raw := range []string{"https://example.test", "http://example.test/path"} {
		if err := validateBaseURL("TEST_URL", raw); err != nil {
			t.Errorf("validateBaseURL(%q) error = %v", raw, err)
		}
	}
	for _, raw := range []string{"", "example.test", "ftp://example.test", "https://"} {
		if err := validateBaseURL("TEST_URL", raw); err == nil {
			t.Errorf("validateBaseURL(%q) accepted an invalid URL", raw)
		}
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("MARKET_DATA_PROVIDER", "simulator")
	t.Setenv("MARKET_DATA_EXCHANGE", "NSE")
	t.Setenv("MARKET_DATA_BHAVCOPY_ENABLED", "false")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.MarketData.Provider != "simulator" {
		t.Errorf("Provider = %q, want simulator", cfg.MarketData.Provider)
	}
	if cfg.MarketData.BhavcopyEnabled {
		t.Error("BhavcopyEnabled = true, want false")
	}
}

func TestLoadFromEnvRejectsInvalidMarketData(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("MARKET_DATA_PROVIDER", "not-a-provider")

	if _, err := LoadFromEnv(); err == nil {
		t.Fatal("LoadFromEnv() accepted an invalid provider")
	}
}
