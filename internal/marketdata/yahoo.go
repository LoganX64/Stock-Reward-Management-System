package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	yahooChartPath      = "/v8/finance/chart/"
	yahooChartQuery     = "interval=1d&range=1d"
	yahooMarketTimeZone = "Asia/Kolkata"
)

// YahooOptions configures the Yahoo Finance chart provider.
type YahooOptions struct {
	BaseURL string
	Mapper  *SymbolMapper
	HTTP    HTTPOptions
	Now     func() time.Time
}

// YahooProvider implements QuoteProvider with Yahoo Finance's chart endpoint.
// It relies on the shared HTTPClient for retry/backoff, rate limiting, and body
// limits.
type YahooProvider struct {
	baseURL string
	mapper  *SymbolMapper
	http    *HTTPClient
	now     func() time.Time
}

var _ QuoteProvider = (*YahooProvider)(nil)

// NewYahooProvider creates an NSE/BSE Yahoo quote provider. An empty BaseURL
// defaults to query1.finance.yahoo.com and a nil Mapper defaults to NSE.
func NewYahooProvider(opts YahooOptions) (*YahooProvider, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://query1.finance.yahoo.com"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("yahoo: invalid base URL %q: %w", baseURL, err)
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("yahoo: invalid base URL %q", baseURL)
	}

	mapper := opts.Mapper
	if mapper == nil {
		mapper, err = NewSymbolMapper(ExchangeNSE, "")
		if err != nil {
			return nil, err
		}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	httpOpts := opts.HTTP
	if httpOpts.Now == nil {
		httpOpts.Now = now
	}

	return &YahooProvider{
		baseURL: baseURL,
		mapper:  mapper,
		http:    NewHTTPClient(SourceYahoo, httpOpts),
		now:     now,
	}, nil
}

// Name returns the provider source name.
func (p *YahooProvider) Name() string {
	return SourceYahoo
}

// GetQuote fetches one delayed Yahoo quote for a canonical application symbol.
func (p *YahooProvider) GetQuote(ctx context.Context, symbol string) (Quote, error) {
	canonical, err := p.mapper.Canonical(symbol)
	if err != nil {
		return Quote{}, err
	}
	providerSymbol, err := p.mapper.ProviderSymbol(canonical)
	if err != nil {
		return Quote{}, err
	}

	endpoint := p.baseURL + yahooChartPath + url.PathEscape(providerSymbol) + "?" + yahooChartQuery
	body, err := p.http.Get(ctx, endpoint, "application/json", YahooBodyLimit)
	if err != nil {
		return Quote{}, err
	}

	quote, err := p.parseQuote(body, canonical, providerSymbol)
	if err != nil {
		return Quote{}, fmt.Errorf("yahoo: %s: %w", providerSymbol, err)
	}
	return quote, nil
}

type yahooChartResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency             string      `json:"currency"`
				Symbol               string      `json:"symbol"`
				FullExchangeName     string      `json:"fullExchangeName"`
				ExchangeTimezoneName string      `json:"exchangeTimezoneName"`
				RegularMarketPrice   json.Number `json:"regularMarketPrice"`
				RegularMarketTime    *int64      `json:"regularMarketTime"`
			} `json:"meta"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

func (p *YahooProvider) parseQuote(body []byte, canonical, providerSymbol string) (Quote, error) {
	var payload yahooChartResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "body",
			Reason:   fmt.Sprintf("invalid JSON: %v", err),
		}
	}

	if payload.Chart.Error != nil {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "chart.error",
			Reason:   yahooErrorMessage(payload.Chart.Error.Code, payload.Chart.Error.Description),
		}
	}
	if len(payload.Chart.Result) == 0 {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "result",
			Reason:   "response contains no chart result",
		}
	}

	meta := payload.Chart.Result[0].Meta
	if meta.Symbol != providerSymbol {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "symbol",
			Reason:   fmt.Sprintf("returned %q, want %q", meta.Symbol, providerSymbol),
		}
	}

	currency := NormalizeCurrency(meta.Currency)
	if currency != CurrencyINR {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "currency",
			Reason:   fmt.Sprintf("returned %q, want INR", meta.Currency),
		}
	}

	exchange, ok := NormalizeExchange(meta.FullExchangeName)
	if !ok {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "exchange",
			Reason:   fmt.Sprintf("returned unsupported exchange %q", meta.FullExchangeName),
		}
	}
	if exchange != p.mapper.Exchange() {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "exchange",
			Reason:   fmt.Sprintf("returned %s, want %s", exchange, p.mapper.Exchange()),
		}
	}

	if tz := strings.TrimSpace(meta.ExchangeTimezoneName); tz != "" {
		switch tz {
		case yahooMarketTimeZone, "Asia/Calcutta":
		default:
			return Quote{}, &ValidationError{
				Provider: SourceYahoo,
				Field:    "exchange_timezone",
				Reason:   fmt.Sprintf("returned %q, want %s", tz, yahooMarketTimeZone),
			}
		}
	}

	priceText := strings.TrimSpace(meta.RegularMarketPrice.String())
	if priceText == "" || strings.EqualFold(priceText, "null") {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "regular_market_price",
			Reason:   "price is required",
		}
	}
	price, err := decimal.NewFromString(priceText)
	if err != nil {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "regular_market_price",
			Reason:   fmt.Sprintf("invalid decimal %q", priceText),
		}
	}

	if meta.RegularMarketTime == nil || *meta.RegularMarketTime <= 0 {
		return Quote{}, &ValidationError{
			Provider: SourceYahoo,
			Field:    "regular_market_time",
			Reason:   "quote time is required",
		}
	}
	asOf := time.Unix(*meta.RegularMarketTime, 0).UTC()
	fetchedAt := p.now().UTC()

	quote := Quote{
		Symbol:         canonical,
		ProviderSymbol: providerSymbol,
		Price:          price,
		Currency:       currency,
		Exchange:       exchange,
		AsOf:           asOf,
		FetchedAt:      fetchedAt,
		Source:         SourceYahoo,
	}
	if err := quote.Validate(fetchedAt); err != nil {
		return Quote{}, err
	}
	return quote, nil
}

func yahooErrorMessage(code, description string) string {
	code = strings.TrimSpace(code)
	description = strings.TrimSpace(description)
	switch {
	case code != "" && description != "":
		return code + ": " + description
	case code != "":
		return code
	case description != "":
		return description
	default:
		return "provider returned an error"
	}
}
