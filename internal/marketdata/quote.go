package marketdata

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/utils"
)

const (
	CurrencyINR = "INR"

	ExchangeNSE = "NSE"
	ExchangeBSE = "BSE"

	SourceYahoo       = "yahoo"
	SourceNSEBhavcopy = "nse_bhavcopy"
	SourceSimulator   = "simulator"
	SourceLegacy      = "legacy"

	DefaultQuoteFutureSkew = 5 * time.Minute
	DefaultQuoteMaxAge     = 7 * 24 * time.Hour
)

// Quote is one price observation from a provider. AsOf is the provider's
// quote time; FetchedAt is the time Stocky received it. Both are absolute
// instants and providers should return them in UTC.
type Quote struct {
	Symbol         string
	ProviderSymbol string
	Price          decimal.Decimal
	Currency       string
	Exchange       string
	AsOf           time.Time
	FetchedAt      time.Time
	Source         string
}

// Validate enforces the shared quote contract. Provider-specific parsers must
// call this before returning a live quote.
func (q Quote) Validate(now time.Time) error {
	return q.validate(now, true)
}

// ValidateHistory enforces the same quote contract for a historical close
// except for the maximum age. A backfill intentionally imports dates older than
// DefaultQuoteMaxAge; future timestamps and every other invariant are still
// rejected. Live quotes must keep using Validate.
func (q Quote) ValidateHistory(now time.Time) error {
	return q.validate(now, false)
}

func (q Quote) validate(now time.Time, enforceMaxAge bool) error {
	if now.IsZero() {
		now = time.Now()
	}

	if q.Symbol == "" || q.Symbol != utils.NormalizeStockSymbol(q.Symbol) {
		return &ValidationError{Provider: q.Source, Field: "symbol", Reason: "must be a non-blank canonical symbol"}
	}
	if q.ProviderSymbol == "" || strings.ContainsAny(q.ProviderSymbol, " \t\r\n") {
		return &ValidationError{Provider: q.Source, Field: "provider_symbol", Reason: "must be non-blank and contain no whitespace"}
	}
	if !q.Price.IsPositive() {
		return &ValidationError{Provider: q.Source, Field: "price", Reason: "must be greater than zero"}
	}
	if q.Currency != CurrencyINR {
		return &ValidationError{Provider: q.Source, Field: "currency", Reason: "must be INR"}
	}
	if q.Exchange != ExchangeNSE && q.Exchange != ExchangeBSE {
		return &ValidationError{Provider: q.Source, Field: "exchange", Reason: "must be NSE or BSE"}
	}
	if q.AsOf.IsZero() {
		return &ValidationError{Provider: q.Source, Field: "as_of", Reason: "quote time is required"}
	}
	if q.AsOf.After(now.Add(DefaultQuoteFutureSkew)) {
		return &ValidationError{Provider: q.Source, Field: "as_of", Reason: "quote time is too far in the future"}
	}
	if enforceMaxAge && q.AsOf.Before(now.Add(-DefaultQuoteMaxAge)) {
		return &ValidationError{Provider: q.Source, Field: "as_of", Reason: "quote time is older than the accepted window"}
	}
	if q.FetchedAt.IsZero() {
		return &ValidationError{Provider: q.Source, Field: "fetched_at", Reason: "fetch time is required"}
	}

	switch q.Source {
	case SourceYahoo, SourceNSEBhavcopy, SourceSimulator:
	default:
		return &ValidationError{Provider: q.Source, Field: "source", Reason: "must be a known market-data source"}
	}

	return nil
}

// NormalizeExchange maps provider exchange names to the canonical NSE/BSE
// values used by the database and API.
func NormalizeExchange(name string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "NSE", "NSE INDIA", "NATIONAL STOCK EXCHANGE", "NATIONAL STOCK EXCHANGE OF INDIA":
		return ExchangeNSE, true
	case "BSE", "BSE INDIA", "BOMBAY STOCK EXCHANGE", "BOMBAY STOCK EXCHANGE LIMITED":
		return ExchangeBSE, true
	default:
		return "", false
	}
}

// NormalizeCurrency returns the canonical uppercase currency code.
func NormalizeCurrency(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}
