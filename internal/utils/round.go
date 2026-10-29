package utils

import (
	"math"

	"github.com/shopspring/decimal"
)

func init() {
	// The API contract uses JSON numbers, not quoted decimal strings. Every
	// handler imports this package, so the setting applies before responses are
	// marshaled.
	decimal.MarshalJSONWithoutQuotes = true
}

// QuantityScale is the storage and API precision for share quantities.
const QuantityScale int32 = 6

// AmountScale is the storage and API precision for INR amounts and prices.
const AmountScale int32 = 4

// RoundQuantity rounds a quantity to 6 decimal places using half-away-from-zero
// rounding. This matches PostgreSQL numeric rounding.
func RoundQuantity(q decimal.Decimal) decimal.Decimal {
	return q.Round(QuantityScale)
}

// RoundAmount rounds an INR amount to 4 decimal places using
// half-away-from-zero rounding. This matches PostgreSQL numeric rounding.
func RoundAmount(a decimal.Decimal) decimal.Decimal {
	return a.Round(AmountScale)
}

// RoundAmountFloat is only for the simulated price updater, whose random-walk
// generator operates on float64. Financial writes use RoundAmount instead.
func RoundAmountFloat(a float64) float64 {
	return math.Round(a*1e4) / 1e4
}
