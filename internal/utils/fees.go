package utils

import "github.com/shopspring/decimal"

// Stocky models a simplified buy-side fee schedule. The rates below are
// documented in readme.md and edge-cases.md. They are deliberately not the
// full Indian statutory schedule: exchange transaction charges, SEBI turnover
// fees, stamp duty, DP charges, sell-side STT, and the statutory GST base are
// out of scope for this MVP.
var (
	BrokerageRate = decimal.RequireFromString("0.005") // 0.5% of notional
	STTRate       = decimal.RequireFromString("0.001") // 0.1% of notional
	GSTRate       = decimal.RequireFromString("0.18")  // 18% of rounded brokerage + STT
)

// FeeBreakdown is the fee component of a reward transaction. Every field is
// rounded to 4 decimal places. Total is the rounded sum of the three rounded
// components.
type FeeBreakdown struct {
	Brokerage decimal.Decimal
	STT       decimal.Decimal
	GST       decimal.Decimal
	Total     decimal.Decimal
}

// CalculateFees applies the documented fee schedule to an already-rounded
// notional amount. Fees are rounded at each step, and GST is calculated on the
// rounded brokerage and STT values rather than on their unrounded products.
func CalculateFees(notional decimal.Decimal) FeeBreakdown {
	brokerage := RoundAmount(notional.Mul(BrokerageRate))
	stt := RoundAmount(notional.Mul(STTRate))
	gst := RoundAmount(brokerage.Add(stt).Mul(GSTRate))

	return FeeBreakdown{
		Brokerage: brokerage,
		STT:       stt,
		GST:       gst,
		Total:     RoundAmount(brokerage.Add(stt).Add(gst)),
	}
}
