package marketdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func yahooTestMapper(t *testing.T, exchange string) *SymbolMapper {
	t.Helper()

	mapper, err := NewSymbolMapper(exchange, "")
	if err != nil {
		t.Fatalf("NewSymbolMapper(%q) error = %v", exchange, err)
	}
	return mapper
}

func yahooTestJSON(price string, epoch int64, currency, exchange, symbol, timezone string) string {
	return fmt.Sprintf(
		`{"chart":{"result":[{"meta":{"currency":%q,"symbol":%q,"fullExchangeName":%q,"exchangeTimezoneName":%q,"regularMarketPrice":%s,"regularMarketTime":%d}}],"error":null}}`,
		currency, symbol, exchange, timezone, price, epoch,
	)
}

func newTestYahooProvider(
	t *testing.T,
	handler http.HandlerFunc,
	exchange string,
	now time.Time,
	mutate func(*HTTPOptions),
) (*YahooProvider, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	httpOptions := HTTPOptions{
		Timeout:       2 * time.Second,
		MaxAttempts:   3,
		RetryBase:     time.Millisecond,
		MaxRetryDelay: 5 * time.Millisecond,
		Jitter:        func(d time.Duration) time.Duration { return d },
	}
	if mutate != nil {
		mutate(&httpOptions)
	}

	provider, err := NewYahooProvider(YahooOptions{
		BaseURL: server.URL,
		Mapper:  yahooTestMapper(t, exchange),
		HTTP:    httpOptions,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewYahooProvider() error = %v", err)
	}
	return provider, server
}

func TestYahooProviderGetQuote(t *testing.T) {
	now := time.Date(2026, time.October, 30, 10, 0, 30, 0, time.UTC)
	quoteTime := time.Date(2026, time.October, 30, 9, 58, 16, 0, time.UTC)

	var gotPath, gotInterval, gotRange, gotAccept string
	provider, _ := newTestYahooProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotInterval = r.URL.Query().Get("interval")
		gotRange = r.URL.Query().Get("range")
		gotAccept = r.Header.Get("Accept")
		_, _ = fmt.Fprint(w, yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "NSE", "TCS.NS", "Asia/Kolkata"))
	}, ExchangeNSE, now, nil)

	quote, err := provider.GetQuote(context.Background(), " tcs ")
	if err != nil {
		t.Fatalf("GetQuote() error = %v", err)
	}

	if gotPath != "/v8/finance/chart/TCS.NS" {
		t.Errorf("request path = %q, want /v8/finance/chart/TCS.NS", gotPath)
	}
	if gotInterval != "1d" || gotRange != "1d" {
		t.Errorf("request query = interval=%q range=%q, want both 1d", gotInterval, gotRange)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
	if quote.Symbol != "TCS" {
		t.Errorf("Symbol = %q, want TCS", quote.Symbol)
	}
	if quote.ProviderSymbol != "TCS.NS" {
		t.Errorf("ProviderSymbol = %q, want TCS.NS", quote.ProviderSymbol)
	}
	if !quote.Price.Equal(decimal.RequireFromString("2502.60")) {
		t.Errorf("Price = %s, want 2502.60", quote.Price)
	}
	if quote.Currency != CurrencyINR {
		t.Errorf("Currency = %q, want INR", quote.Currency)
	}
	if quote.Exchange != ExchangeNSE {
		t.Errorf("Exchange = %q, want NSE", quote.Exchange)
	}
	if !quote.AsOf.Equal(quoteTime) {
		t.Errorf("AsOf = %s, want %s", quote.AsOf, quoteTime)
	}
	if !quote.FetchedAt.Equal(now) {
		t.Errorf("FetchedAt = %s, want %s", quote.FetchedAt, now)
	}
	if quote.Source != SourceYahoo {
		t.Errorf("Source = %q, want yahoo", quote.Source)
	}
}

func TestYahooProviderUsesBSESymbol(t *testing.T) {
	now := time.Date(2026, time.October, 30, 10, 0, 30, 0, time.UTC)
	quoteTime := now.Add(-2 * time.Minute)

	var gotPath string
	provider, _ := newTestYahooProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = fmt.Fprint(w, yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "BSE", "TCS.BO", "Asia/Kolkata"))
	}, ExchangeBSE, now, nil)

	quote, err := provider.GetQuote(context.Background(), "TCS")
	if err != nil {
		t.Fatalf("GetQuote() error = %v", err)
	}
	if gotPath != "/v8/finance/chart/TCS.BO" {
		t.Errorf("request path = %q, want /v8/finance/chart/TCS.BO", gotPath)
	}
	if quote.Exchange != ExchangeBSE {
		t.Errorf("Exchange = %q, want BSE", quote.Exchange)
	}
	if quote.ProviderSymbol != "TCS.BO" {
		t.Errorf("ProviderSymbol = %q, want TCS.BO", quote.ProviderSymbol)
	}
}

func TestYahooProviderRejectsInvalidResponses(t *testing.T) {
	now := time.Date(2026, time.October, 30, 10, 0, 30, 0, time.UTC)
	quoteTime := now.Add(-2 * time.Minute)
	valid := func() string {
		return yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "NSE", "TCS.NS", "Asia/Kolkata")
	}

	tests := []struct {
		name      string
		payload   string
		wantField string
	}{
		{name: "malformed JSON", payload: `{`, wantField: "body"},
		{
			name:      "chart error",
			payload:   `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found"}}}`,
			wantField: "chart.error",
		},
		{name: "empty result", payload: `{"chart":{"result":[],"error":null}}`, wantField: "result"},
		{
			name:      "missing price",
			payload:   fmt.Sprintf(`{"chart":{"result":[{"meta":{"currency":"INR","symbol":"TCS.NS","fullExchangeName":"NSE","regularMarketTime":%d}}],"error":null}}`, quoteTime.Unix()),
			wantField: "regular_market_price",
		},
		{
			name:      "null price",
			payload:   fmt.Sprintf(`{"chart":{"result":[{"meta":{"currency":"INR","symbol":"TCS.NS","fullExchangeName":"NSE","regularMarketPrice":null,"regularMarketTime":%d}}],"error":null}}`, quoteTime.Unix()),
			wantField: "regular_market_price",
		},
		{
			name:      "zero price",
			payload:   yahooTestJSON("0", quoteTime.Unix(), "INR", "NSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "price",
		},
		{
			name:      "negative price",
			payload:   yahooTestJSON("-1", quoteTime.Unix(), "INR", "NSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "price",
		},
		{
			name:      "missing time",
			payload:   `{"chart":{"result":[{"meta":{"currency":"INR","symbol":"TCS.NS","fullExchangeName":"NSE","regularMarketPrice":2502.60}}],"error":null}}`,
			wantField: "regular_market_time",
		},
		{
			name:      "zero time",
			payload:   yahooTestJSON("2502.60", 0, "INR", "NSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "regular_market_time",
		},
		{
			name:      "wrong currency",
			payload:   yahooTestJSON("2502.60", quoteTime.Unix(), "USD", "NSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "currency",
		},
		{
			name:      "wrong exchange",
			payload:   yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "NYSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "exchange",
		},
		{
			name:      "configured exchange mismatch",
			payload:   yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "BSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "exchange",
		},
		{
			name:      "wrong symbol",
			payload:   yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "NSE", "INFY.NS", "Asia/Kolkata"),
			wantField: "symbol",
		},
		{
			name:      "wrong timezone",
			payload:   yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "NSE", "TCS.NS", "America/New_York"),
			wantField: "exchange_timezone",
		},
		{
			name:      "future quote",
			payload:   yahooTestJSON("2502.60", now.Add(10*time.Minute).Unix(), "INR", "NSE", "TCS.NS", "Asia/Kolkata"),
			wantField: "as_of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := tt.payload
			if payload == "" {
				payload = valid()
			}
			provider, _ := newTestYahooProvider(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, payload)
			}, ExchangeNSE, now, nil)

			_, err := provider.GetQuote(context.Background(), "TCS")
			if err == nil {
				t.Fatal("GetQuote() accepted an invalid response")
			}
			if !IsValidationError(err) {
				t.Fatalf("GetQuote() error = %T %v, want *ValidationError", err, err)
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("GetQuote() error = %T, want *ValidationError", err)
			}
			if validationErr.Field != tt.wantField {
				t.Errorf("validation field = %q, want %q", validationErr.Field, tt.wantField)
			}
		})
	}
}

func TestYahooProviderDoesNotRetry400(t *testing.T) {
	now := time.Date(2026, time.October, 30, 10, 0, 30, 0, time.UTC)
	var attempts int32

	provider, _ := newTestYahooProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}, ExchangeNSE, now, nil)

	_, err := provider.GetQuote(context.Background(), "TCS")
	if err == nil {
		t.Fatal("GetQuote() accepted HTTP 400")
	}
	if !IsHTTPError(err) {
		t.Fatalf("GetQuote() error = %T %v, want *HTTPError", err, err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestYahooProviderRetriesTransientStatus(t *testing.T) {
	now := time.Date(2026, time.October, 30, 10, 0, 30, 0, time.UTC)
	quoteTime := now.Add(-2 * time.Minute)
	var attempts int32
	var sleeps []time.Duration

	provider, _ := newTestYahooProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, yahooTestJSON("2502.60", quoteTime.Unix(), "INR", "NSE", "TCS.NS", "Asia/Kolkata"))
	}, ExchangeNSE, now, func(options *HTTPOptions) {
		options.Sleep = func(ctx context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		}
	})

	quote, err := provider.GetQuote(context.Background(), "TCS")
	if err != nil {
		t.Fatalf("GetQuote() error = %v", err)
	}
	if !quote.Price.Equal(decimal.RequireFromString("2502.60")) {
		t.Errorf("Price = %s, want 2502.60", quote.Price)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if len(sleeps) != 2 {
		t.Errorf("sleep count = %d, want 2", len(sleeps))
	}
}

func TestYahooProviderRejectsInvalidBaseURL(t *testing.T) {
	if _, err := NewYahooProvider(YahooOptions{BaseURL: "not-a-url"}); err == nil {
		t.Fatal("NewYahooProvider() accepted an invalid base URL")
	}
}

func TestYahooProviderLive(t *testing.T) {
	if os.Getenv("MARKET_DATA_LIVE_TEST") != "1" {
		t.Skip("set MARKET_DATA_LIVE_TEST=1 to run the live Yahoo smoke test")
	}

	provider, err := NewYahooProvider(YahooOptions{
		BaseURL: "https://query1.finance.yahoo.com",
		HTTP: HTTPOptions{
			Timeout:     10 * time.Second,
			MaxAttempts: 3,
			RetryBase:   time.Second,
			RateLimit:   time.Second,
		},
	})
	if err != nil {
		t.Fatalf("NewYahooProvider() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, symbol := range []string{"TCS", "INFY", "RELIANCE"} {
		quote, err := provider.GetQuote(ctx, symbol)
		if err != nil {
			t.Fatalf("GetQuote(%s) error = %v", symbol, err)
		}
		if quote.Currency != CurrencyINR {
			t.Errorf("%s currency = %q, want INR", symbol, quote.Currency)
		}
		if quote.Exchange != ExchangeNSE {
			t.Errorf("%s exchange = %q, want NSE", symbol, quote.Exchange)
		}
		if !quote.Price.IsPositive() {
			t.Errorf("%s price = %s, want positive", symbol, quote.Price)
		}
		t.Logf("%s %s %s %s", symbol, quote.Price, quote.AsOf.Format(time.RFC3339), quote.ProviderSymbol)
	}
}
