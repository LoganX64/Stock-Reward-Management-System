package marketdata

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func validQuote(now time.Time) Quote {
	return Quote{
		Symbol:         "TCS",
		ProviderSymbol: "TCS.NS",
		Price:          decimal.RequireFromString("2502.60"),
		Currency:       CurrencyINR,
		Exchange:       ExchangeNSE,
		AsOf:           now.Add(-time.Minute),
		FetchedAt:      now,
		Source:         SourceYahoo,
	}
}

func TestQuoteValidate(t *testing.T) {
	now := time.Date(2026, time.November, 2, 10, 0, 0, 0, time.UTC)
	if err := validQuote(now).Validate(now); err != nil {
		t.Fatalf("valid quote rejected: %v", err)
	}
}

func TestQuoteValidateRejectsInvalidValues(t *testing.T) {
	now := time.Date(2026, time.November, 2, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		edit func(*Quote)
	}{
		{name: "lowercase symbol", edit: func(q *Quote) { q.Symbol = "tcs" }},
		{name: "blank symbol", edit: func(q *Quote) { q.Symbol = "" }},
		{name: "provider symbol whitespace", edit: func(q *Quote) { q.ProviderSymbol = "TCS NS" }},
		{name: "blank provider symbol", edit: func(q *Quote) { q.ProviderSymbol = "" }},
		{name: "zero price", edit: func(q *Quote) { q.Price = decimal.Zero }},
		{name: "negative price", edit: func(q *Quote) { q.Price = decimal.NewFromInt(-1) }},
		{name: "wrong currency", edit: func(q *Quote) { q.Currency = "USD" }},
		{name: "wrong exchange", edit: func(q *Quote) { q.Exchange = "NYSE" }},
		{name: "zero as of", edit: func(q *Quote) { q.AsOf = time.Time{} }},
		{name: "future as of", edit: func(q *Quote) { q.AsOf = now.Add(6 * time.Minute) }},
		{name: "old as of", edit: func(q *Quote) { q.AsOf = now.Add(-8 * 24 * time.Hour) }},
		{name: "zero fetched at", edit: func(q *Quote) { q.FetchedAt = time.Time{} }},
		{name: "unknown source", edit: func(q *Quote) { q.Source = "legacy" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quote := validQuote(now)
			tt.edit(&quote)
			err := quote.Validate(now)
			if err == nil {
				t.Fatal("Validate() accepted an invalid quote")
			}
			if !IsValidationError(err) {
				t.Fatalf("Validate() error = %T, want *ValidationError", err)
			}
		})
	}
}

func TestNormalizeExchange(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "NSE", want: ExchangeNSE, ok: true},
		{input: " nse ", want: ExchangeNSE, ok: true},
		{input: "National Stock Exchange of India", want: ExchangeNSE, ok: true},
		{input: "BSE", want: ExchangeBSE, ok: true},
		{input: "Bombay Stock Exchange", want: ExchangeBSE, ok: true},
		{input: "NYSE", ok: false},
		{input: "", ok: false},
	}

	for _, tt := range tests {
		got, ok := NormalizeExchange(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("NormalizeExchange(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNormalizeCurrency(t *testing.T) {
	if got := NormalizeCurrency(" inr "); got != CurrencyINR {
		t.Errorf("NormalizeCurrency() = %q, want %q", got, CurrencyINR)
	}
}

func TestProviderErrorHelpers(t *testing.T) {
	validation := &ValidationError{Provider: "yahoo", Field: "price", Reason: "bad"}
	if !IsValidationError(validation) {
		t.Error("IsValidationError() = false, want true")
	}
	if IsHTTPError(validation) {
		t.Error("IsHTTPError(validation) = true, want false")
	}
	if IsValidationError(errors.New("x")) {
		t.Error("IsValidationError(plain error) = true, want false")
	}

	httpErr := &HTTPError{Provider: "yahoo", StatusCode: 500, URL: "https://example.test"}
	if !IsHTTPError(httpErr) {
		t.Error("IsHTTPError() = false, want true")
	}
	if IsHTTPError(errors.New("x")) {
		t.Error("IsHTTPError(plain error) = true, want false")
	}

	if !IsNoSession(fmt.Errorf("wrapped: %w", ErrNoSession)) {
		t.Error("IsNoSession(wrapped) = false, want true")
	}
	if IsNoSession(errors.New("wrapped: " + ErrNoSession.Error())) {
		t.Error("IsNoSession() matched plain text, want false")
	}
}
