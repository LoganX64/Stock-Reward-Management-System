package marketdata

import (
	"strings"
	"testing"
)

func TestSymbolMapperNSEMapping(t *testing.T) {
	mapper, err := NewSymbolMapper("nse", "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}

	tests := []struct {
		input     string
		canonical string
		provider  string
	}{
		{input: "TCS", canonical: "TCS", provider: "TCS.NS"},
		{input: " tcs ", canonical: "TCS", provider: "TCS.NS"},
		{input: "M&M", canonical: "M&M", provider: "M&M.NS"},
		{input: "BAJAJ-AUTO", canonical: "BAJAJ-AUTO", provider: "BAJAJ-AUTO.NS"},
		{input: "3MINDIA", canonical: "3MINDIA", provider: "3MINDIA.NS"},
	}

	for _, tt := range tests {
		canonical, err := mapper.Canonical(tt.input)
		if err != nil {
			t.Errorf("Canonical(%q) error = %v", tt.input, err)
			continue
		}
		if canonical != tt.canonical {
			t.Errorf("Canonical(%q) = %q, want %q", tt.input, canonical, tt.canonical)
		}
		provider, err := mapper.ProviderSymbol(tt.input)
		if err != nil {
			t.Errorf("ProviderSymbol(%q) error = %v", tt.input, err)
			continue
		}
		if provider != tt.provider {
			t.Errorf("ProviderSymbol(%q) = %q, want %q", tt.input, provider, tt.provider)
		}
	}
}

func TestSymbolMapperBSEMapping(t *testing.T) {
	mapper, err := NewSymbolMapper("BSE", "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}
	if mapper.Exchange() != ExchangeBSE {
		t.Errorf("Exchange() = %q, want %q", mapper.Exchange(), ExchangeBSE)
	}
	provider, err := mapper.ProviderSymbol("tcs")
	if err != nil {
		t.Fatalf("ProviderSymbol() error = %v", err)
	}
	if provider != "TCS.BO" {
		t.Errorf("ProviderSymbol() = %q, want TCS.BO", provider)
	}
}

func TestSymbolMapperExplicitSuffix(t *testing.T) {
	mapper, err := NewSymbolMapper("NSE", ".NSX")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}
	provider, err := mapper.ProviderSymbol("TCS")
	if err != nil {
		t.Fatalf("ProviderSymbol() error = %v", err)
	}
	if provider != "TCS.NSX" {
		t.Errorf("ProviderSymbol() = %q, want TCS.NSX", provider)
	}
}

func TestSymbolMapperRejectsInvalidInput(t *testing.T) {
	mapper, err := NewSymbolMapper("NSE", "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}

	for _, symbol := range []string{"", " ", "T CS", "TC/S", "T@CS", "TCS.NS", strings.Repeat("A", 33)} {
		if _, err := mapper.Canonical(symbol); err == nil {
			t.Errorf("Canonical(%q) accepted an invalid symbol", symbol)
		}
	}
}

func TestSymbolMapperRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		exchange string
		suffix   string
	}{
		{name: "bad exchange", exchange: "NYSE"},
		{name: "suffix without dot", exchange: "NSE", suffix: "NS"},
		{name: "suffix whitespace", exchange: "NSE", suffix: ".N S"},
		{name: "suffix slash", exchange: "NSE", suffix: ".N/S"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewSymbolMapper(tt.exchange, tt.suffix); err == nil {
				t.Fatal("NewSymbolMapper() accepted invalid configuration")
			}
		})
	}
}

func TestSymbolMapperEscapedProviderSymbol(t *testing.T) {
	mapper, err := NewSymbolMapper("NSE", "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}
	escaped, err := mapper.EscapedProviderSymbol("M&M")
	if err != nil {
		t.Fatalf("EscapedProviderSymbol() error = %v", err)
	}
	if escaped == "" {
		t.Fatal("EscapedProviderSymbol() returned an empty string")
	}
}

func TestBhavcopySymbol(t *testing.T) {
	mapper, err := NewSymbolMapper("NSE", "")
	if err != nil {
		t.Fatalf("NewSymbolMapper() error = %v", err)
	}
	got, err := mapper.BhavcopySymbol(" tcs ")
	if err != nil {
		t.Fatalf("BhavcopySymbol() error = %v", err)
	}
	if got != "TCS" {
		t.Errorf("BhavcopySymbol() = %q, want TCS", got)
	}

	if _, err := BhavcopySymbol("tcs"); err != nil {
		t.Errorf("standalone BhavcopySymbol() error = %v", err)
	}

	bse, err := NewSymbolMapper("BSE", "")
	if err != nil {
		t.Fatalf("NewSymbolMapper(BSE) error = %v", err)
	}
	if _, err := bse.BhavcopySymbol("TCS"); err == nil {
		t.Fatal("BhavcopySymbol() accepted a BSE mapper")
	}
}
