package marketdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QuoteProvider returns one current quote for a canonical application symbol.
// Implementations must never return a fabricated price together with an error.
type QuoteProvider interface {
	Name() string
	GetQuote(ctx context.Context, symbol string) (Quote, error)
}

// DailyCloseProvider returns official daily closes for a trading date, keyed
// by canonical application symbol.
type DailyCloseProvider interface {
	Name() string
	GetDailyCloses(ctx context.Context, date time.Time, symbols []string) (map[string]Quote, error)
}

// ErrNoSession means a provider has no data for the requested trading session
// yet, for example because an exchange file has not been published.
var ErrNoSession = errors.New("marketdata: no session data available")

// ValidationError means a provider response was received but failed the quote
// contract. It is never retryable.
type ValidationError struct {
	Provider string
	Field    string
	Reason   string
}

func (e *ValidationError) Error() string {
	prefix := "marketdata"
	if e.Provider != "" {
		prefix = e.Provider
	}
	return fmt.Sprintf("%s: validation failed for %s: %s", prefix, e.Field, e.Reason)
}

// HTTPError describes a non-2xx provider response.
type HTTPError struct {
	Provider    string
	StatusCode  int
	URL         string
	RetryAfter  time.Duration
	BodySnippet string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("%s: GET %s: HTTP %d", e.Provider, e.URL, e.StatusCode)
	if e.BodySnippet != "" {
		msg += ": " + e.BodySnippet
	}
	return msg
}

// IsRetryableStatus reports whether an HTTP status is safe to retry.
func IsRetryableStatus(status int) bool {
	return status == 408 || status == 429 || status >= 500
}

// IsValidationError reports whether err is a provider-response validation
// failure.
func IsValidationError(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}

// IsHTTPError reports whether err is a non-2xx provider response.
func IsHTTPError(err error) bool {
	var target *HTTPError
	return errors.As(err, &target)
}

// IsNoSession reports whether err means the requested session is not
// available yet.
func IsNoSession(err error) bool {
	return errors.Is(err, ErrNoSession)
}

func providerName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "marketdata"
	}
	return name
}
