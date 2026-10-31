// Package marketcal provides the Indian equity market calendar and shared
// price-freshness policy. Business dates are always expressed in
// Asia/Kolkata (IST, UTC+05:30, no daylight saving).
package marketcal

import (
	"fmt"
	"strings"
	"time"

	// Embed the timezone database so the Alpine runtime image can resolve
	// Asia/Kolkata without installing tzdata.
	_ "time/tzdata"
)

const (
	MarketTimeZone = "Asia/Kolkata"
	dateLayout     = "2006-01-02"

	sessionOpenHour    = 9
	sessionOpenMinute  = 15
	sessionCloseHour   = 15
	sessionCloseMinute = 30
)

// nse2026Holidays is the list of full-day NSE closures published for 2026.
// Muhurat sessions (for example 8 November 2026) are intentionally not
// modelled; they are documented as unsupported by the A3 plan.
var nse2026Holidays = []string{
	"2026-01-26",
	"2026-03-03",
	"2026-03-26",
	"2026-03-31",
	"2026-04-03",
	"2026-04-14",
	"2026-05-01",
	"2026-05-28",
	"2026-06-26",
	"2026-09-14",
	"2026-10-02",
	"2026-10-20",
	"2026-11-10",
	"2026-11-24",
	"2026-12-25",
}

var marketLocation = mustLoadLocation(MarketTimeZone)

// defaultCalendar shares the immutable default holiday set across callers.
var defaultCalendar = mustNewCalendar(nil)

// Location returns the fixed market timezone.
func Location() *time.Location {
	return marketLocation
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("marketcal: load timezone %q: %v", name, err))
	}
	return loc
}

// Calendar answers trading-day and trading-session questions for the Indian
// equity market. A Calendar is safe for concurrent use after construction.
type Calendar struct {
	loc      *time.Location
	holidays map[string]struct{}
}

// New builds a calendar containing the built-in NSE 2026 holidays plus the
// supplied YYYY-MM-DD overrides.
func New(overrides []string) (*Calendar, error) {
	return newCalendar(overrides)
}

func newCalendar(overrides []string) (*Calendar, error) {
	c := &Calendar{
		loc:      marketLocation,
		holidays: make(map[string]struct{}, len(nse2026Holidays)+len(overrides)),
	}

	for _, raw := range nse2026Holidays {
		key, err := normalizeHoliday(raw)
		if err != nil {
			return nil, err
		}
		c.holidays[key] = struct{}{}
	}
	for _, raw := range overrides {
		key, err := normalizeHoliday(raw)
		if err != nil {
			return nil, err
		}
		c.holidays[key] = struct{}{}
	}

	return c, nil
}

func mustNewCalendar(overrides []string) *Calendar {
	c, err := newCalendar(overrides)
	if err != nil {
		panic(fmt.Sprintf("marketcal: build default calendar: %v", err))
	}
	return c
}

// Default returns the built-in calendar with no extra closures.
func Default() *Calendar {
	return defaultCalendar
}

// DefaultWithOverrides returns the built-in calendar plus MARKET_HOLIDAYS
// style closures.
func DefaultWithOverrides(overrides []string) (*Calendar, error) {
	return newCalendar(overrides)
}

func normalizeHoliday(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	t, err := time.ParseInLocation(dateLayout, raw, marketLocation)
	if err != nil {
		return "", fmt.Errorf("marketcal: invalid holiday %q (want YYYY-MM-DD): %w", raw, err)
	}
	return t.Format(dateLayout), nil
}

// Location returns the calendar's timezone.
func (c *Calendar) Location() *time.Location {
	if c == nil || c.loc == nil {
		return marketLocation
	}
	return c.loc
}

// Date returns midnight IST on the market date containing t.
func (c *Calendar) Date(t time.Time) time.Time {
	local := t.In(c.Location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.Location())
}

// IsHoliday reports whether t falls on a configured full-day closure.
func (c *Calendar) IsHoliday(t time.Time) bool {
	local := t.In(c.Location())
	_, ok := c.holidays[local.Format(dateLayout)]
	return ok
}

// IsTradingDay reports whether t is a weekday that is not a configured NSE
// holiday.
func (c *Calendar) IsTradingDay(t time.Time) bool {
	local := t.In(c.Location())
	switch local.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return !c.IsHoliday(local)
}

// SessionOpen returns 09:15 IST on the market date containing t. On a
// non-trading day this is the nominal session open for that date.
func (c *Calendar) SessionOpen(t time.Time) time.Time {
	local := t.In(c.Location())
	return time.Date(
		local.Year(), local.Month(), local.Day(),
		sessionOpenHour, sessionOpenMinute, 0, 0,
		c.Location(),
	)
}

// SessionClose returns 15:30 IST on the market date containing t. On a
// non-trading day this is the nominal session close for that date.
func (c *Calendar) SessionClose(t time.Time) time.Time {
	local := t.In(c.Location())
	return time.Date(
		local.Year(), local.Month(), local.Day(),
		sessionCloseHour, sessionCloseMinute, 0, 0,
		c.Location(),
	)
}

// IsOpen reports whether t is inside a regular trading session. The session
// is half-open: 09:15 inclusive, 15:30 exclusive.
func (c *Calendar) IsOpen(t time.Time) bool {
	if !c.IsTradingDay(t) {
		return false
	}
	local := t.In(c.Location())
	return !local.Before(c.SessionOpen(local)) && local.Before(c.SessionClose(local))
}

// PreviousSessionClose returns the 15:30 IST close of the most recent trading
// day before the market date containing t. It never returns today's close.
func (c *Calendar) PreviousSessionClose(t time.Time) time.Time {
	day := c.Date(t)
	for i := 0; i < 400; i++ {
		day = day.AddDate(0, 0, -1)
		if c.IsTradingDay(day) {
			return c.SessionClose(day)
		}
	}
	return time.Time{}
}

// LastCompletedSessionClose returns the close of the most recent session that
// has finished at t. During a session it returns the previous session close;
// at or after 15:30 on a trading day it returns today's close.
func (c *Calendar) LastCompletedSessionClose(t time.Time) time.Time {
	if c.IsTradingDay(t) {
		close := c.SessionClose(t)
		if !t.In(c.Location()).Before(close) {
			return close
		}
	}
	return c.PreviousSessionClose(t)
}
