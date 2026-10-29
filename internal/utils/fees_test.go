package utils

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func TestCalculateFees(t *testing.T) {
	fees := CalculateFees(decimal.NewFromInt(200))

	assertDecimal(t, "brokerage", fees.Brokerage, "1")
	assertDecimal(t, "stt", fees.STT, "0.2")
	assertDecimal(t, "gst", fees.GST, "0.216")
	assertDecimal(t, "total", fees.Total, "1.416")
}

func TestCalculateFeesRoundsAtEachStep(t *testing.T) {
	fees := CalculateFees(decimal.RequireFromString("1.2345"))

	// brokerage = 0.0061725 -> 0.0062
	// stt       = 0.0012345 -> 0.0012
	// gst       = (0.0062 + 0.0012) * 0.18 = 0.001332 -> 0.0013
	assertDecimal(t, "brokerage", fees.Brokerage, "0.0062")
	assertDecimal(t, "stt", fees.STT, "0.0012")
	assertDecimal(t, "gst", fees.GST, "0.0013")
	assertDecimal(t, "total", fees.Total, "0.0087")
}

func TestCalculateFeesKeepsLargeDecimalPrecision(t *testing.T) {
	fees := CalculateFees(decimal.RequireFromString("12345678901234.5678"))

	assertDecimal(t, "brokerage", fees.Brokerage, "61728394506.1728")
	assertDecimal(t, "stt", fees.STT, "12345678901.2346")
	assertDecimal(t, "gst", fees.GST, "13333333213.3333")
	assertDecimal(t, "total", fees.Total, "87407406620.7407")
}

func TestRoundAmountHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"0.00015", "0.0002"},
		{"2.00005", "2.0001"},
		{"-0.00015", "-0.0002"},
		{"-2.00005", "-2.0001"},
		{"0.00005", "0.0001"},
		{"0.00004", "0"},
	}

	for _, tc := range cases {
		got := RoundAmount(decimal.RequireFromString(tc.value))
		assertDecimal(t, tc.value, got, tc.want)
	}
}

func TestRoundQuantityHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"0.0000005", "0.000001"},
		{"0.0000004", "0"},
		{"-0.0000005", "-0.000001"},
	}

	for _, tc := range cases {
		got := RoundQuantity(decimal.RequireFromString(tc.value))
		assertDecimal(t, tc.value, got, tc.want)
	}
}

func TestRoundAmountKeepsExactDatabaseValue(t *testing.T) {
	value := decimal.RequireFromString("12345678901234.5678")
	if !RoundAmount(value).Equal(value) {
		t.Errorf("RoundAmount(%s) = %s, want unchanged", value, RoundAmount(value))
	}
}

func TestDecimalMarshalsAsJSONNumber(t *testing.T) {
	encoded, err := json.Marshal(map[string]any{
		"amount": decimal.RequireFromString("1.4160"),
	})
	if err != nil {
		t.Fatalf("marshal decimal: %v", err)
	}
	if string(encoded) != `{"amount":1.416}` {
		t.Errorf("decimal JSON = %s, want {\"amount\":1.416}", encoded)
	}
}

func assertDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	expected := decimal.RequireFromString(want)
	if !got.Equal(expected) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}
