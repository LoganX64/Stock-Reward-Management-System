package marketdata

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/LoganX64/stocky-api/internal/utils"
)

// symbolPattern is intentionally conservative. Indian NSE symbols can contain
// digits, ampersands, and hyphens (for example 3MINDIA, M&M, BAJAJ-AUTO), but
// not whitespace, slashes, or provider suffixes.
var symbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9&\-]{0,31}$`)

// SymbolMapper maps canonical Stocky symbols to provider symbols. It keeps the
// same mapping rules in one place so Yahoo and bhavcopy cannot drift.
type SymbolMapper struct {
	exchange string
	suffix   string
}

// NewSymbolMapper creates a mapper for NSE or BSE. An empty suffix is derived
// from the exchange (.NS for NSE, .BO for BSE). A non-empty suffix is treated
// as an explicit override.
func NewSymbolMapper(exchange, suffix string) (*SymbolMapper, error) {
	ex := strings.ToUpper(strings.TrimSpace(exchange))
	switch ex {
	case ExchangeNSE, ExchangeBSE:
	default:
		return nil, fmt.Errorf("marketdata: unsupported exchange %q (want NSE or BSE)", exchange)
	}

	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		suffix = defaultSuffix(ex)
	}
	if !strings.HasPrefix(suffix, ".") || len(suffix) < 2 {
		return nil, fmt.Errorf("marketdata: invalid symbol suffix %q (want a value such as .NS or .BO)", suffix)
	}
	if strings.ContainsAny(suffix, " /\\\t\r\n") {
		return nil, fmt.Errorf("marketdata: invalid symbol suffix %q", suffix)
	}

	return &SymbolMapper{exchange: ex, suffix: suffix}, nil
}

func defaultSuffix(exchange string) string {
	if exchange == ExchangeBSE {
		return ".BO"
	}
	return ".NS"
}

// Exchange returns the canonical exchange for this mapper.
func (m *SymbolMapper) Exchange() string {
	if m == nil {
		return ""
	}
	return m.exchange
}

// Suffix returns the configured Yahoo-style suffix.
func (m *SymbolMapper) Suffix() string {
	if m == nil {
		return ""
	}
	return m.suffix
}

// Canonical normalizes and validates an application symbol.
func (m *SymbolMapper) Canonical(symbol string) (string, error) {
	canonical := utils.NormalizeStockSymbol(symbol)
	if !symbolPattern.MatchString(canonical) {
		return "", fmt.Errorf("marketdata: invalid instrument symbol %q", symbol)
	}
	return canonical, nil
}

// ProviderSymbol returns the symbol to send to a Yahoo-style provider.
func (m *SymbolMapper) ProviderSymbol(symbol string) (string, error) {
	canonical, err := m.Canonical(symbol)
	if err != nil {
		return "", err
	}
	return canonical + m.suffix, nil
}

// YahooSymbol is an alias for ProviderSymbol for readability at call sites.
func (m *SymbolMapper) YahooSymbol(symbol string) (string, error) {
	return m.ProviderSymbol(symbol)
}

// EscapedProviderSymbol returns the URL path form of ProviderSymbol.
func (m *SymbolMapper) EscapedProviderSymbol(symbol string) (string, error) {
	providerSymbol, err := m.ProviderSymbol(symbol)
	if err != nil {
		return "", err
	}
	return url.PathEscape(providerSymbol), nil
}

// BhavcopySymbol returns the bare NSE symbol used by the official bhavcopy
// file. Bhavcopy is NSE-only.
func (m *SymbolMapper) BhavcopySymbol(symbol string) (string, error) {
	if m.exchange != ExchangeNSE {
		return "", fmt.Errorf("marketdata: bhavcopy is only available for NSE, not %s", m.exchange)
	}
	return m.Canonical(symbol)
}

// BhavcopySymbol validates a bare NSE symbol without needing an exchange
// mapper.
func BhavcopySymbol(symbol string) (string, error) {
	canonical := utils.NormalizeStockSymbol(symbol)
	if !symbolPattern.MatchString(canonical) {
		return "", fmt.Errorf("marketdata: invalid instrument symbol %q", symbol)
	}
	return canonical, nil
}
