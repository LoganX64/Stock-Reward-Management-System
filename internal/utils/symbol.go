package utils

import "strings"

// NormalizeStockSymbol returns the canonical instrument symbol used by both the
// API and the database: leading/trailing ASCII whitespace is removed, then the
// value is uppercased. It matches the PostgreSQL function
// normalize_instrument_symbol(TEXT).
func NormalizeStockSymbol(symbol string) string {
	return strings.ToUpper(strings.Trim(symbol, " \t\n\r"))
}
