package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestSimulatorGetQuote(t *testing.T) {
	now := time.Date(2026, time.November, 2, 4, 30, 0, 0, time.UTC)
	simulator, err := NewSimulatorWithOptions(SimulatorOptions{
		Now:        func() time.Time { return now },
		Seed:       42,
		Volatility: 0.01,
		BasePrice:  decimal.NewFromInt(1000),
	})
	if err != nil {
		t.Fatalf("NewSimulatorWithOptions() error = %v", err)
	}
	if simulator.Name() != SourceSimulator {
		t.Errorf("Name() = %q, want %q", simulator.Name(), SourceSimulator)
	}

	quote, err := simulator.GetQuote(context.Background(), " tcs ")
	if err != nil {
		t.Fatalf("GetQuote() error = %v", err)
	}
	if quote.Symbol != "TCS" {
		t.Errorf("Symbol = %q, want TCS", quote.Symbol)
	}
	if quote.ProviderSymbol != "TCS.NS" {
		t.Errorf("ProviderSymbol = %q, want TCS.NS", quote.ProviderSymbol)
	}
	if !quote.Price.IsPositive() {
		t.Errorf("Price = %s, want positive", quote.Price)
	}
	if quote.Currency != CurrencyINR {
		t.Errorf("Currency = %q, want INR", quote.Currency)
	}
	if quote.Exchange != ExchangeNSE {
		t.Errorf("Exchange = %q, want NSE", quote.Exchange)
	}
	if quote.Source != SourceSimulator {
		t.Errorf("Source = %q, want simulator", quote.Source)
	}
	if !quote.AsOf.Equal(now) || !quote.FetchedAt.Equal(now) {
		t.Errorf("timestamps = (%s, %s), want %s", quote.AsOf, quote.FetchedAt, now)
	}
	if err := quote.Validate(now); err != nil {
		t.Errorf("simulator quote rejected: %v", err)
	}
}

func TestSimulatorDeterministic(t *testing.T) {
	now := time.Date(2026, time.November, 2, 4, 30, 0, 0, time.UTC)
	options := SimulatorOptions{
		Now:        func() time.Time { return now },
		Seed:       7,
		Volatility: 0.02,
	}
	first, err := NewSimulatorWithOptions(options)
	if err != nil {
		t.Fatalf("first NewSimulatorWithOptions() error = %v", err)
	}
	second, err := NewSimulatorWithOptions(options)
	if err != nil {
		t.Fatalf("second NewSimulatorWithOptions() error = %v", err)
	}

	firstQuote, err := first.GetQuote(context.Background(), "TCS")
	if err != nil {
		t.Fatalf("first GetQuote() error = %v", err)
	}
	secondQuote, err := second.GetQuote(context.Background(), "TCS")
	if err != nil {
		t.Fatalf("second GetQuote() error = %v", err)
	}
	if !firstQuote.Price.Equal(secondQuote.Price) {
		t.Errorf("same seed produced %s and %s", firstQuote.Price, secondQuote.Price)
	}
}

func TestSimulatorBSE(t *testing.T) {
	now := time.Date(2026, time.November, 2, 4, 30, 0, 0, time.UTC)
	simulator, err := NewSimulatorWithOptions(SimulatorOptions{
		Exchange: "BSE",
		Now:      func() time.Time { return now },
		Seed:     1,
	})
	if err != nil {
		t.Fatalf("NewSimulatorWithOptions() error = %v", err)
	}
	quote, err := simulator.GetQuote(context.Background(), "TCS")
	if err != nil {
		t.Fatalf("GetQuote() error = %v", err)
	}
	if quote.Exchange != ExchangeBSE {
		t.Errorf("Exchange = %q, want BSE", quote.Exchange)
	}
	if quote.ProviderSymbol != "TCS.BO" {
		t.Errorf("ProviderSymbol = %q, want TCS.BO", quote.ProviderSymbol)
	}
}

func TestSimulatorFailureInjection(t *testing.T) {
	now := time.Date(2026, time.November, 2, 4, 30, 0, 0, time.UTC)
	simulator, err := NewSimulatorWithOptions(SimulatorOptions{
		Now:         func() time.Time { return now },
		Seed:        1,
		FailureRate: 1,
	})
	if err != nil {
		t.Fatalf("NewSimulatorWithOptions() error = %v", err)
	}
	quote, err := simulator.GetQuote(context.Background(), "TCS")
	if !errors.Is(err, ErrSimulatedFailure) {
		t.Fatalf("GetQuote() error = %v, want ErrSimulatedFailure", err)
	}
	if quote.Symbol != "" {
		t.Errorf("GetQuote() returned a quote with a failure: %+v", quote)
	}
}

func TestSimulatorContextCancellation(t *testing.T) {
	simulator := NewSimulator()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := simulator.GetQuote(ctx, "TCS"); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetQuote() error = %v, want context.Canceled", err)
	}
}

func TestSimulatorInvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts SimulatorOptions
	}{
		{name: "bad exchange", opts: SimulatorOptions{Exchange: "NYSE"}},
		{name: "negative volatility", opts: SimulatorOptions{Volatility: -0.1}},
		{name: "high volatility", opts: SimulatorOptions{Volatility: 0.6}},
		{name: "negative failure rate", opts: SimulatorOptions{FailureRate: -0.1}},
		{name: "high failure rate", opts: SimulatorOptions{FailureRate: 1.1}},
		{name: "negative base price", opts: SimulatorOptions{BasePrice: decimal.NewFromInt(-1)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewSimulatorWithOptions(tt.opts); err == nil {
				t.Fatal("NewSimulatorWithOptions() accepted invalid options")
			}
		})
	}
}
