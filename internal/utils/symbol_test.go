package utils

import "testing"

func TestNormalizeStockSymbol(t *testing.T) {
	tests := []struct {
		name   string
		symbol string
		want   string
	}{
		{name: "already canonical", symbol: "TCS", want: "TCS"},
		{name: "lowercase", symbol: "tcs", want: "TCS"},
		{name: "mixed case", symbol: "TcS", want: "TCS"},
		{name: "surrounding spaces", symbol: "  tcs  ", want: "TCS"},
		{name: "tabs and newlines", symbol: "\t tcs \r\n", want: "TCS"},
		{name: "internal spaces preserved", symbol: "t cs", want: "T CS"},
		{name: "empty", symbol: "", want: ""},
		{name: "whitespace only", symbol: " \t\r\n", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeStockSymbol(tt.symbol); got != tt.want {
				t.Errorf("NormalizeStockSymbol(%q) = %q, want %q", tt.symbol, got, tt.want)
			}
		})
	}
}
