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

// MarketData holds A3 market-data configuration. The live price path is not
// wired to these values until Phase 3.
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

type Config struct {
	Env           string
	Database      Database
	HTTPServer    HTTPServer
	MigrationPath string
	MarketData    MarketData
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
