package utils

import "math"

func RoundQuantity(q float64) float64 {
	return math.Round(q*1e6) / 1e6
}

func RoundAmount(a float64) float64 {
	return math.Round(a*1e4) / 1e4
}
