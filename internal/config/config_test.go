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
