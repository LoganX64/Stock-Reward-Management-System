package marketdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/marketcal"
)

var bhavcopyTestNow = time.Date(2026, time.October, 30, 18, 15, 0, 0, marketcal.Location())

const bhavcopyTestHeader = "SYMBOL, SERIES, DATE1, PREV_CLOSE, OPEN_PRICE, HIGH_PRICE, LOW_PRICE, LAST_PRICE, CLOSE_PRICE, AVG_PRICE, TTL_TRD_QNTY, TURNOVER_LACS, NO_OF_TRADES, DELIV_QTY, DELIV_PER\r\n"

func newTestBhavcopyProvider(
	t *testing.T,
	handler http.HandlerFunc,
	now time.Time,
) (*BhavcopyProvider, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	mapper, err := NewSymbolMapper(ExchangeNSE, "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}

	provider, err := NewBhavcopyProvider(BhavcopyOptions{
		BaseURL: server.URL,
		Mapper:  mapper,
		HTTP:    testHTTPOptions(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewBhavcopyProvider() error = %v", err)
	}
	return provider, server
}

func bhavcopyTestRows() string {
	return bhavcopyTestHeader +
		"20MICRONS, EQ, 30-Oct-2026, 168.09, 166.51, 166.79, 159.00, 161.57, 161.48, 161.92, 132501, 214.54, 3365, 60324, 45.53\r\n" +
		"TCS, BE, 30-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, 9999.00, 2513.04, 3016983, 75818.09, 119512, 1420735, 47.09\r\n" +
		"INFY, EQ, 30-Oct-2026, 1146.50, 1149.10, 1185.00, 1147.90, 1185.00, 1185.00, 1174.76, 13030713, 153080.21, 184733, 7575174, 58.13\r\n" +
		"RELIANCE, EQ, 30-Oct-2026, 1167.40, 1170.40, 1174.60, 1152.50, 1164.90, 1164.90, 1160.33, 14753650, 171191.72, 207450, 8073955, 54.73\r\n" +
		"TCS, EQ, 30-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, 2502.60, 2513.04, 3016983, 75818.09, 119512, 1420735, 47.09\r\n"
}

func TestBhavcopyProviderGetDailyCloses(t *testing.T) {
	var (
		requests  int32
		gotPath   string
		gotAccept string
	)
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("\ufeff" + bhavcopyTestRows()))
	}, bhavcopyTestNow)

	quotes, err := provider.GetDailyCloses(
		context.Background(),
		bhavcopyTestNow,
		[]string{" tcs ", "INFY", "RELIANCE"},
	)
	if err != nil {
		t.Fatalf("GetDailyCloses() error = %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("HTTP requests = %d, want 1", got)
	}
	if gotPath != "/products/content/sec_bhavdata_full_30102026.csv" {
		t.Errorf("request path = %q, want the 30 Oct 2026 bhavcopy file", gotPath)
	}
	if !strings.Contains(gotAccept, "text/csv") {
		t.Errorf("Accept = %q, want text/csv", gotAccept)
	}

	want := map[string]string{
		"TCS":      "2502.60",
		"INFY":     "1185.00",
		"RELIANCE": "1164.90",
	}
	if len(quotes) != len(want) {
		t.Fatalf("quotes = %d, want %d", len(quotes), len(want))
	}

	wantAsOf := time.Date(2026, time.October, 30, 15, 30, 0, 0, marketcal.Location()).UTC()
	for symbol, price := range want {
		quote, ok := quotes[symbol]
		if !ok {
			t.Fatalf("missing quote for %s", symbol)
		}
		if !quote.Price.Equal(decimal.RequireFromString(price)) {
			t.Errorf("%s price = %s, want %s", symbol, quote.Price, price)
		}
		if quote.Symbol != symbol {
			t.Errorf("%s Symbol = %q, want %q", symbol, quote.Symbol, symbol)
		}
		if quote.ProviderSymbol != symbol {
			t.Errorf("%s ProviderSymbol = %q, want bare NSE symbol", symbol, quote.ProviderSymbol)
		}
		if quote.Currency != CurrencyINR {
			t.Errorf("%s Currency = %q, want INR", symbol, quote.Currency)
		}
		if quote.Exchange != ExchangeNSE {
			t.Errorf("%s Exchange = %q, want NSE", symbol, quote.Exchange)
		}
		if quote.Source != SourceNSEBhavcopy {
			t.Errorf("%s Source = %q, want %s", symbol, quote.Source, SourceNSEBhavcopy)
		}
		if !quote.AsOf.Equal(wantAsOf) {
			t.Errorf("%s AsOf = %s, want %s", symbol, quote.AsOf, wantAsOf)
		}
		if !quote.FetchedAt.Equal(bhavcopyTestNow.UTC()) {
			t.Errorf("%s FetchedAt = %s, want %s", symbol, quote.FetchedAt, bhavcopyTestNow.UTC())
		}
	}
}

func TestBhavcopyProviderDuplicateSymbolFirstWins(t *testing.T) {
	body := bhavcopyTestHeader +
		"TCS, EQ, 30-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, 2502.60, 2513.04, 1, 1, 1, 1, 1\r\n" +
		"TCS, EQ, 30-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, 9999.00, 2513.04, 1, 1, 1, 1, 1\r\n"

	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, bhavcopyTestNow)

	quotes, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err != nil {
		t.Fatalf("GetDailyCloses() error = %v", err)
	}
	if got := quotes["TCS"].Price; !got.Equal(decimal.RequireFromString("2502.60")) {
		t.Errorf("TCS price = %s, want the first duplicate row 2502.60", got)
	}
}

func TestBhavcopyProviderSkipsInvalidClosePrices(t *testing.T) {
	body := bhavcopyTestHeader +
		"TCS, EQ, 30-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, -, 2513.04, 1, 1, 1, 1, 1\r\n" +
		"INFY, EQ, 30-Oct-2026, 1146.50, 1149.10, 1185.00, 1147.90, 1185.00, not-a-price, 1174.76, 1, 1, 1, 1, 1\r\n" +
		"RELIANCE, EQ, 30-Oct-2026, 1167.40, 1170.40, 1174.60, 1152.50, 1164.90, 1164.90, 1160.33, 1, 1, 1, 1, 1\r\n"

	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, bhavcopyTestNow)

	quotes, err := provider.GetDailyCloses(
		context.Background(),
		bhavcopyTestNow,
		[]string{"TCS", "INFY", "RELIANCE"},
	)
	if err != nil {
		t.Fatalf("GetDailyCloses() error = %v", err)
	}
	if _, ok := quotes["TCS"]; ok {
		t.Error("TCS with a '-' close price should be omitted")
	}
	if _, ok := quotes["INFY"]; ok {
		t.Error("INFY with a non-numeric close price should be omitted")
	}
	if _, ok := quotes["RELIANCE"]; !ok {
		t.Error("RELIANCE with a valid close price should be present")
	}
}

func TestBhavcopyProviderRejectsDateMismatch(t *testing.T) {
	body := bhavcopyTestHeader +
		"TCS, EQ, 29-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, 2502.60, 2513.04, 1, 1, 1, 1, 1\r\n"

	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() accepted a file for the wrong date")
	}
	if !IsValidationError(err) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "DATE1" {
		t.Errorf("validation error = %+v, want field DATE1", validationErr)
	}
}

func TestBhavcopyProviderRejectsMissingRequiredHeader(t *testing.T) {
	body := "SYMBOL, SERIES, DATE1, PREV_CLOSE\r\nTCS, EQ, 30-Oct-2026, 2478.00\r\n"

	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() accepted a file without CLOSE_PRICE")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "header" {
		t.Fatalf("validation error = %+v, want field header", validationErr)
	}
	if !strings.Contains(validationErr.Reason, "CLOSE_PRICE") {
		t.Errorf("reason = %q, want it to mention CLOSE_PRICE", validationErr.Reason)
	}
}

func TestBhavcopyProviderRejectsHTMLBody(t *testing.T) {
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>Access denied</body></html>"))
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() accepted an HTML response")
	}
	if !IsValidationError(err) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
}

func TestBhavcopyProviderRejectsMalformedCSV(t *testing.T) {
	body := bhavcopyTestHeader +
		"TCS, EQ, 30-Oct-2026, \"2502.60\r\n"

	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() accepted malformed CSV")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "csv" {
		t.Fatalf("validation error = %+v, want field csv", validationErr)
	}
}

func TestBhavcopyProviderReturnsNoSessionOn404(t *testing.T) {
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() returned nil error for HTTP 404")
	}
	if !IsNoSession(err) {
		t.Errorf("error = %v, want ErrNoSession", err)
	}
	if !IsHTTPError(err) {
		t.Errorf("error = %v, want wrapped HTTPError details", err)
	}
}

func TestBhavcopyProviderEmptySymbolsMakesNoRequest(t *testing.T) {
	var requests int32
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
	}, bhavcopyTestNow)

	quotes, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, nil)
	if err != nil {
		t.Fatalf("GetDailyCloses() error = %v", err)
	}
	if len(quotes) != 0 {
		t.Errorf("quotes = %d, want 0", len(quotes))
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("HTTP requests = %d, want 0", got)
	}
}

func TestBhavcopyProviderRequiresNSE(t *testing.T) {
	mapper, err := NewSymbolMapper(ExchangeBSE, "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}

	if _, err := NewBhavcopyProvider(BhavcopyOptions{Mapper: mapper}); err == nil {
		t.Fatal("NewBhavcopyProvider() accepted a BSE mapper")
	}
}

func TestBhavcopyProviderRejectsInvalidBaseURL(t *testing.T) {
	if _, err := NewBhavcopyProvider(BhavcopyOptions{BaseURL: "not-a-url"}); err == nil {
		t.Fatal("NewBhavcopyProvider() accepted an invalid base URL")
	}
}

func TestBhavcopyProviderLive(t *testing.T) {
	if os.Getenv("MARKET_DATA_LIVE_TEST") != "1" {
		t.Skip("set MARKET_DATA_LIVE_TEST=1 to run the live NSE bhavcopy smoke test")
	}

	provider, err := NewBhavcopyProvider(BhavcopyOptions{
		BaseURL: "https://nsearchives.nseindia.com",
		HTTP: HTTPOptions{
			Timeout:     20 * time.Second,
			MaxAttempts: 3,
			RetryBase:   time.Second,
			RateLimit:   time.Second,
		},
	})
	if err != nil {
		t.Fatalf("NewBhavcopyProvider() error = %v", err)
	}

	calendar := marketcal.Default()
	date := calendar.PreviousSessionClose(time.Now())
	if date.IsZero() {
		t.Fatal("could not determine the previous NSE session")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	symbols := []string{"TCS", "INFY", "RELIANCE"}
	quotes, err := provider.GetDailyCloses(ctx, date, symbols)
	if err != nil {
		t.Fatalf("GetDailyCloses(%s) error = %v", date.Format("2006-01-02"), err)
	}
	for _, symbol := range symbols {
		quote, ok := quotes[symbol]
		if !ok {
			t.Errorf("missing live bhavcopy quote for %s", symbol)
			continue
		}
		if !quote.Price.IsPositive() {
			t.Errorf("%s price = %s, want positive", symbol, quote.Price)
		}
		t.Logf("%s %s %s %s", symbol, quote.Price, quote.AsOf.Format(time.RFC3339), quote.ProviderSymbol)
	}
}

func TestBhavcopyProviderRejectsZeroDate(t *testing.T) {
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP request must not be made for a zero date")
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), time.Time{}, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() accepted a zero date")
	}
	if !IsValidationError(err) {
		t.Errorf("error = %T %v, want *ValidationError", err, err)
	}
}

func TestBhavcopyProviderDoesNotEnforceTradingDay(t *testing.T) {
	// The calendar decides which dates the service requests. The provider only
	// validates DATE1 against the requested date, so a direct call for a
	// non-trading day still parses a matching file.
	body := bhavcopyTestHeader +
		"TCS, EQ, 31-Oct-2026, 2478.00, 2495.20, 2527.70, 2494.20, 2502.60, 2502.60, 2513.04, 1, 1, 1, 1, 1\r\n"

	requested := time.Date(2026, time.October, 31, 0, 0, 0, 0, marketcal.Location())
	fetchedAt := time.Date(2026, time.October, 31, 18, 15, 0, 0, marketcal.Location())
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, fetchedAt)

	quotes, err := provider.GetDailyCloses(context.Background(), requested, []string{"TCS"})
	if err != nil {
		t.Fatalf("GetDailyCloses() error = %v", err)
	}
	if got := quotes["TCS"].Price; !got.Equal(decimal.RequireFromString("2502.60")) {
		t.Errorf("TCS price = %s, want 2502.60", got)
	}
}

func TestBhavcopyProviderErrorMentionsProvider(t *testing.T) {
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}, bhavcopyTestNow)

	_, err := provider.GetDailyCloses(context.Background(), bhavcopyTestNow, []string{"TCS"})
	if err == nil {
		t.Fatal("GetDailyCloses() returned nil error for HTTP 403")
	}
	if !strings.Contains(err.Error(), SourceNSEBhavcopy) {
		t.Errorf("error = %q, want provider name", err)
	}
	if !IsHTTPError(err) {
		t.Errorf("error = %T %v, want *HTTPError", err, err)
	}
}

func TestBhavcopyProviderFetchesOneFileForAllSymbols(t *testing.T) {
	var requests int32
	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = fmt.Fprint(w, bhavcopyTestRows())
	}, bhavcopyTestNow)

	if _, err := provider.GetDailyCloses(
		context.Background(),
		bhavcopyTestNow,
		[]string{"TCS", "INFY", "RELIANCE", "TCS"},
	); err != nil {
		t.Fatalf("GetDailyCloses() error = %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("HTTP requests = %d, want 1 for a batch request", got)
	}
}

func TestBhavcopyProviderHistoricalClosesSkipLiveMaxAge(t *testing.T) {
	historicalDate := time.Date(2020, time.January, 2, 15, 30, 0, 0, marketcal.Location())
	rows := bhavcopyTestHeader +
		"TCS, EQ, 02-Jan-2020, 2080.00, 2090.00, 2110.00, 2075.00, 2100.00, 2100.50, 2095.00, 1000, 10.00, 100, 500, 50.00\r\n"

	provider, _ := newTestBhavcopyProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(rows))
	}, bhavcopyTestNow)

	if _, err := provider.GetDailyCloses(context.Background(), historicalDate, []string{"TCS"}); err == nil {
		t.Fatal("live GetDailyCloses accepted a quote older than the maximum age")
	}

	quotes, err := provider.GetHistoricalDailyCloses(context.Background(), historicalDate, []string{"TCS"})
	if err != nil {
		t.Fatalf("GetHistoricalDailyCloses() error = %v", err)
	}
	quote, ok := quotes["TCS"]
	if !ok {
		t.Fatal("GetHistoricalDailyCloses() did not return TCS")
	}
	if !quote.Price.Equal(decimal.RequireFromString("2100.50")) {
		t.Errorf("historical close = %s, want the raw close 2100.50", quote.Price)
	}
	if quote.Source != SourceNSEBhavcopy {
		t.Errorf("historical source = %q, want %q", quote.Source, SourceNSEBhavcopy)
	}
	if !quote.AsOf.Equal(time.Date(2020, time.January, 2, 15, 30, 0, 0, marketcal.Location()).UTC()) {
		t.Errorf("historical AsOf = %s, want the 2020 session close", quote.AsOf)
	}
}
