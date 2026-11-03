package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const defaultMarketUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36 Stocky/1.0"

type HTTPServer struct {
	Port string
}

type Database struct {
	Host     string
	DbPort   string
	User     string
	Password string
	DBName   string
	SSLMode  string
}

// MarketData holds A3 market-data configuration consumed by the live price
// scheduler and providers.
type MarketData struct {
	Enabled         bool
	Provider        string
	Exchange        string
	SymbolSuffix    string
	TickInterval    time.Duration
	MaxStale        time.Duration
	CloseGrace      time.Duration
	MarketHoursOnly bool
	FinalQuoteDelay time.Duration

	HTTPTimeout time.Duration
	MaxRetries  int
	RetryBase   time.Duration
	RateLimit   time.Duration
	UserAgent   string

	YahooBaseURL string

	BhavcopyEnabled       bool
	BhavcopyBaseURL       string
	BhavcopyTime          string
	BhavcopyRetryUntil    string
	BhavcopyRetryInterval time.Duration

	Holidays []string
}

// Projection holds the write-path hardening settings for the maintained
// read projections: queue worker cadence, retry/alert thresholds, rollover
// due-check cadence, and reconciliation sampling.
type Projection struct {
	RefreshInterval             time.Duration
	RefreshBatch                int
	QueueMaxAttempts            int
	QueueRetention              time.Duration
	QueueFailedRetention        time.Duration
	QueueAlertDepth             int
	QueueAlertLag               time.Duration
	ReconciliationInterval      time.Duration
	ReconciliationSamplePercent float64
	ReconciliationMaxUsers      int
	SyncUserLimit               int
	CorporateActionInterval     time.Duration
}

type Config struct {
	Env           string
	Database      Database
	HTTPServer    HTTPServer
	MigrationPath string
	MarketData    MarketData
	Projection    Projection
}

// LoadFromEnv loads .env files and environment variables, returning a
// validated configuration. Invalid values are reported with the variable name.
func LoadFromEnv() (*Config, error) {
	// Load .env file if it exists
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env.local"
	}
	if err := godotenv.Load(envFile); err != nil {
		log.Printf("No %s file found, using system environment variables", envFile)
	}

	getEnv := func(key, defaultVal string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return defaultVal
	}

	marketData, err := loadMarketData(getEnv)
	if err != nil {
		return nil, err
	}

	projection, err := loadProjection(getEnv)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Env: getEnv("ENV", "dev"),
		Database: Database{
			Host:     getEnv("DB_HOST", "localhost"),
			DbPort:   getEnv("DB_PORT", "5432"),
			User:     getEnv("POSTGRES_USER", "postgres"),
			Password: getEnv("POSTGRES_PASSWORD", "password"),
			DBName:   getEnv("POSTGRES_DB", "assignment"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		HTTPServer: HTTPServer{
			Port: ":" + getEnv("HTTP_PORT", "8080"),
		},
		MigrationPath: getEnv("MIGRATION_PATH", "./internal/database/migrations"),
		MarketData:    marketData,
		Projection:    projection,
	}

	return cfg, nil
}

func MustLoad() *Config {
	cfg, err := LoadFromEnv()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	return cfg
}

func loadMarketData(getEnv func(string, string) string) (MarketData, error) {
	md := MarketData{
		Enabled:               true,
		Provider:              "yahoo",
		Exchange:              "NSE",
		TickInterval:          60 * time.Minute,
		MaxStale:              90 * time.Minute,
		CloseGrace:            20 * time.Minute,
		MarketHoursOnly:       true,
		FinalQuoteDelay:       5 * time.Minute,
		HTTPTimeout:           10 * time.Second,
		MaxRetries:            3,
		RetryBase:             time.Second,
		RateLimit:             time.Second,
		UserAgent:             defaultMarketUserAgent,
		YahooBaseURL:          "https://query1.finance.yahoo.com",
		BhavcopyEnabled:       true,
		BhavcopyBaseURL:       "https://nsearchives.nseindia.com",
		BhavcopyTime:          "18:15",
		BhavcopyRetryUntil:    "20:30",
		BhavcopyRetryInterval: 30 * time.Minute,
	}

	var err error

	if md.Enabled, err = getBoolEnv(getEnv, "MARKET_DATA_ENABLED", "true"); err != nil {
		return md, err
	}
	if md.MarketHoursOnly, err = getBoolEnv(getEnv, "MARKET_DATA_MARKET_HOURS_ONLY", "true"); err != nil {
		return md, err
	}
	if md.BhavcopyEnabled, err = getBoolEnv(getEnv, "MARKET_DATA_BHAVCOPY_ENABLED", "true"); err != nil {
		return md, err
	}

	if md.TickInterval, err = getDurationEnv(getEnv, "MARKET_DATA_TICK_INTERVAL", "60m"); err != nil {
		return md, err
	}
	if md.MaxStale, err = getDurationEnv(getEnv, "MARKET_DATA_MAX_STALE", "90m"); err != nil {
		return md, err
	}
	if md.CloseGrace, err = getDurationEnv(getEnv, "MARKET_DATA_CLOSE_GRACE", "20m"); err != nil {
		return md, err
	}
	if md.FinalQuoteDelay, err = getDurationEnv(getEnv, "MARKET_DATA_FINAL_QUOTE_DELAY", "5m"); err != nil {
		return md, err
	}
	if md.HTTPTimeout, err = getDurationEnv(getEnv, "MARKET_DATA_HTTP_TIMEOUT", "10s"); err != nil {
		return md, err
	}
	if md.RetryBase, err = getDurationEnv(getEnv, "MARKET_DATA_RETRY_BASE", "1s"); err != nil {
		return md, err
	}
	if md.RateLimit, err = getDurationEnv(getEnv, "MARKET_DATA_RATE_LIMIT", "1s"); err != nil {
		return md, err
	}
	if md.BhavcopyRetryInterval, err = getDurationEnv(getEnv, "MARKET_DATA_BHAVCOPY_RETRY_INTERVAL", "30m"); err != nil {
		return md, err
	}
	if md.MaxRetries, err = getIntEnv(getEnv, "MARKET_DATA_MAX_RETRIES", "3"); err != nil {
		return md, err
	}

	md.Provider = strings.ToLower(strings.TrimSpace(getEnv("MARKET_DATA_PROVIDER", "yahoo")))
	switch md.Provider {
	case "yahoo", "simulator":
	default:
		return md, fmt.Errorf("MARKET_DATA_PROVIDER: unsupported provider %q (want yahoo or simulator)", md.Provider)
	}

	md.Exchange = strings.ToUpper(strings.TrimSpace(getEnv("MARKET_DATA_EXCHANGE", "NSE")))
	switch md.Exchange {
	case "NSE", "BSE":
	default:
		return md, fmt.Errorf("MARKET_DATA_EXCHANGE: unsupported exchange %q (want NSE or BSE)", md.Exchange)
	}

	md.SymbolSuffix = strings.TrimSpace(getEnv("MARKET_DATA_SYMBOL_SUFFIX", ""))
	if md.SymbolSuffix == "" {
		if md.Exchange == "BSE" {
			md.SymbolSuffix = ".BO"
		} else {
			md.SymbolSuffix = ".NS"
		}
	}
	if !strings.HasPrefix(md.SymbolSuffix, ".") || len(md.SymbolSuffix) < 2 {
		return md, fmt.Errorf("MARKET_DATA_SYMBOL_SUFFIX: invalid suffix %q (want a value such as .NS or .BO)", md.SymbolSuffix)
	}
	if strings.ContainsAny(md.SymbolSuffix, " /\\\t\r\n") {
		return md, fmt.Errorf("MARKET_DATA_SYMBOL_SUFFIX: invalid suffix %q", md.SymbolSuffix)
	}

	md.UserAgent = strings.TrimSpace(getEnv("MARKET_DATA_USER_AGENT", defaultMarketUserAgent))
	if md.UserAgent == "" {
		md.UserAgent = defaultMarketUserAgent
	}

	md.YahooBaseURL = strings.TrimRight(strings.TrimSpace(getEnv("MARKET_DATA_YAHOO_BASE_URL", "https://query1.finance.yahoo.com")), "/")
	if err := validateBaseURL("MARKET_DATA_YAHOO_BASE_URL", md.YahooBaseURL); err != nil {
		return md, err
	}

	md.BhavcopyBaseURL = strings.TrimRight(strings.TrimSpace(getEnv("MARKET_DATA_BHAVCOPY_BASE_URL", "https://nsearchives.nseindia.com")), "/")
	if err := validateBaseURL("MARKET_DATA_BHAVCOPY_BASE_URL", md.BhavcopyBaseURL); err != nil {
		return md, err
	}

	md.BhavcopyTime = strings.TrimSpace(getEnv("MARKET_DATA_BHAVCOPY_TIME", "18:15"))
	firstAttempt, err := parseClock("MARKET_DATA_BHAVCOPY_TIME", md.BhavcopyTime)
	if err != nil {
		return md, err
	}
	md.BhavcopyRetryUntil = strings.TrimSpace(getEnv("MARKET_DATA_BHAVCOPY_RETRY_UNTIL", "20:30"))
	lastAttempt, err := parseClock("MARKET_DATA_BHAVCOPY_RETRY_UNTIL", md.BhavcopyRetryUntil)
	if err != nil {
		return md, err
	}
	if lastAttempt < firstAttempt {
		return md, fmt.Errorf("MARKET_DATA_BHAVCOPY_RETRY_UNTIL: %q must not be before MARKET_DATA_BHAVCOPY_TIME %q", md.BhavcopyRetryUntil, md.BhavcopyTime)
	}

	holidays, err := parseHolidays(getEnv("MARKET_HOLIDAYS", ""))
	if err != nil {
		return md, err
	}
	md.Holidays = holidays

	if md.TickInterval <= 0 {
		return md, fmt.Errorf("MARKET_DATA_TICK_INTERVAL: must be greater than zero")
	}
	if md.MaxStale <= 0 {
		return md, fmt.Errorf("MARKET_DATA_MAX_STALE: must be greater than zero")
	}
	if md.CloseGrace < 0 {
		return md, fmt.Errorf("MARKET_DATA_CLOSE_GRACE: must not be negative")
	}
	if md.FinalQuoteDelay < 0 {
		return md, fmt.Errorf("MARKET_DATA_FINAL_QUOTE_DELAY: must not be negative")
	}
	if md.HTTPTimeout <= 0 {
		return md, fmt.Errorf("MARKET_DATA_HTTP_TIMEOUT: must be greater than zero")
	}
	if md.MaxRetries < 1 || md.MaxRetries > 10 {
		return md, fmt.Errorf("MARKET_DATA_MAX_RETRIES: must be between 1 and 10")
	}
	if md.RetryBase < 0 {
		return md, fmt.Errorf("MARKET_DATA_RETRY_BASE: must not be negative")
	}
	if md.RateLimit < 0 {
		return md, fmt.Errorf("MARKET_DATA_RATE_LIMIT: must not be negative")
	}
	if md.BhavcopyRetryInterval <= 0 {
		return md, fmt.Errorf("MARKET_DATA_BHAVCOPY_RETRY_INTERVAL: must be greater than zero")
	}
	if md.BhavcopyEnabled && md.Exchange != "NSE" {
		return md, fmt.Errorf("MARKET_DATA_BHAVCOPY_ENABLED: bhavcopy requires MARKET_DATA_EXCHANGE=NSE")
	}

	return md, nil
}

func loadProjection(getEnv func(string, string) string) (Projection, error) {
	p := Projection{
		RefreshInterval:             30 * time.Second,
		RefreshBatch:                200,
		QueueMaxAttempts:            5,
		QueueRetention:              168 * time.Hour,
		QueueFailedRetention:        720 * time.Hour,
		QueueAlertDepth:             1000,
		QueueAlertLag:               15 * time.Minute,
		ReconciliationInterval:      time.Hour,
		ReconciliationSamplePercent: 0.1,
		ReconciliationMaxUsers:      500,
		SyncUserLimit:               50,
		CorporateActionInterval:     15 * time.Minute,
	}

	var err error

	if p.RefreshInterval, err = getDurationEnv(getEnv, "PROJECTION_REFRESH_INTERVAL", "30s"); err != nil {
		return p, err
	}
	if p.RefreshBatch, err = getIntEnv(getEnv, "PROJECTION_REFRESH_BATCH", "200"); err != nil {
		return p, err
	}
	if p.QueueMaxAttempts, err = getIntEnv(getEnv, "PROJECTION_QUEUE_MAX_ATTEMPTS", "5"); err != nil {
		return p, err
	}
	if p.QueueRetention, err = getDurationEnv(getEnv, "PROJECTION_QUEUE_RETENTION", "168h"); err != nil {
		return p, err
	}
	if p.QueueFailedRetention, err = getDurationEnv(getEnv, "PROJECTION_QUEUE_FAILED_RETENTION", "720h"); err != nil {
		return p, err
	}
	if p.QueueAlertDepth, err = getIntEnv(getEnv, "PROJECTION_QUEUE_ALERT_DEPTH", "1000"); err != nil {
		return p, err
	}
	if p.QueueAlertLag, err = getDurationEnv(getEnv, "PROJECTION_QUEUE_ALERT_LAG", "15m"); err != nil {
		return p, err
	}
	if p.ReconciliationInterval, err = getDurationEnv(getEnv, "PROJECTION_RECONCILIATION_INTERVAL", "1h"); err != nil {
		return p, err
	}
	if p.ReconciliationSamplePercent, err = getFloatEnv(getEnv, "PROJECTION_RECONCILIATION_SAMPLE_PERCENT", "0.1"); err != nil {
		return p, err
	}
	if p.ReconciliationMaxUsers, err = getIntEnv(getEnv, "PROJECTION_RECONCILIATION_MAX_USERS", "500"); err != nil {
		return p, err
	}
	if p.SyncUserLimit, err = getIntEnv(getEnv, "PROJECTION_SYNC_USER_LIMIT", "50"); err != nil {
		return p, err
	}
	if p.CorporateActionInterval, err = getDurationEnv(getEnv, "CORPORATE_ACTION_INTERVAL", "15m"); err != nil {
		return p, err
	}

	if p.RefreshInterval <= 0 {
		return p, fmt.Errorf("PROJECTION_REFRESH_INTERVAL: must be greater than zero")
	}
	if p.RefreshBatch < 1 {
		return p, fmt.Errorf("PROJECTION_REFRESH_BATCH: must be at least 1")
	}
	if p.QueueMaxAttempts < 1 || p.QueueMaxAttempts > 100 {
		return p, fmt.Errorf("PROJECTION_QUEUE_MAX_ATTEMPTS: must be between 1 and 100")
	}
	if p.QueueRetention <= 0 {
		return p, fmt.Errorf("PROJECTION_QUEUE_RETENTION: must be greater than zero")
	}
	if p.QueueFailedRetention <= 0 {
		return p, fmt.Errorf("PROJECTION_QUEUE_FAILED_RETENTION: must be greater than zero")
	}
	if p.QueueAlertDepth < 1 {
		return p, fmt.Errorf("PROJECTION_QUEUE_ALERT_DEPTH: must be at least 1")
	}
	if p.QueueAlertLag <= 0 {
		return p, fmt.Errorf("PROJECTION_QUEUE_ALERT_LAG: must be greater than zero")
	}
	if p.ReconciliationInterval <= 0 {
		return p, fmt.Errorf("PROJECTION_RECONCILIATION_INTERVAL: must be greater than zero")
	}
	if p.ReconciliationSamplePercent <= 0 || p.ReconciliationSamplePercent > 100 {
		return p, fmt.Errorf("PROJECTION_RECONCILIATION_SAMPLE_PERCENT: must be greater than 0 and at most 100")
	}
	if p.ReconciliationMaxUsers < 1 {
		return p, fmt.Errorf("PROJECTION_RECONCILIATION_MAX_USERS: must be at least 1")
	}
	if p.SyncUserLimit < 0 {
		return p, fmt.Errorf("PROJECTION_SYNC_USER_LIMIT: must not be negative")
	}
	if p.CorporateActionInterval <= 0 {
		return p, fmt.Errorf("CORPORATE_ACTION_INTERVAL: must be greater than zero")
	}

	return p, nil
}

func getBoolEnv(getEnv func(string, string) string, key, defaultVal string) (bool, error) {
	raw := strings.TrimSpace(getEnv(key, defaultVal))
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q: %w", key, raw, err)
	}
	return value, nil
}

func getDurationEnv(getEnv func(string, string) string, key, defaultVal string) (time.Duration, error) {
	raw := strings.TrimSpace(getEnv(key, defaultVal))
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, raw, err)
	}
	return value, nil
}

func getIntEnv(getEnv func(string, string) string, key, defaultVal string) (int, error) {
	raw := strings.TrimSpace(getEnv(key, defaultVal))
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, raw, err)
	}
	return value, nil
}

func getFloatEnv(getEnv func(string, string) string, key, defaultVal string) (float64, error) {
	raw := strings.TrimSpace(getEnv(key, defaultVal))
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid number %q: %w", key, raw, err)
	}
	return value, nil
}

func parseClock(key, raw string) (int, error) {
	parsed, err := time.Parse("15:04", raw)
	if err != nil || parsed.Format("15:04") != raw {
		return 0, fmt.Errorf("%s: invalid time %q (want HH:MM)", key, raw)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func parseHolidays(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	holidays := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := time.ParseInLocation("2006-01-02", part, time.UTC)
		if err != nil || parsed.Format("2006-01-02") != part {
			return nil, fmt.Errorf("MARKET_HOLIDAYS: invalid date %q (want YYYY-MM-DD)", part)
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		holidays = append(holidays, part)
	}
	return holidays, nil
}

func validateBaseURL(key, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL %q: %w", key, raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s: URL %q must use http or https", key, raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s: URL %q must include a host", key, raw)
	}
	return nil
}
