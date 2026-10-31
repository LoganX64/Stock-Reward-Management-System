package marketcal

import "time"

// Freshness is the calendar-aware freshness classification of a current
// price. Missing or legacy prices do not have a freshness value.
type Freshness string

const (
	FreshnessFresh   Freshness = "fresh"
	FreshnessStale   Freshness = "stale"
	FreshnessUnknown Freshness = "unknown"

	// SourceLegacy marks prices that predate provenance tracking. Kept here
	// as a plain string so marketcal does not depend on provider packages.
	SourceLegacy = "legacy"
)

const (
	DefaultMaxStale   = 90 * time.Minute
	DefaultCloseGrace = 20 * time.Minute
	DefaultFutureSkew = 5 * time.Minute
)

// FreshnessPolicy classifies prices using the NSE calendar. The same policy
// is intended for the scheduler and for API responses so the two cannot drift.
type FreshnessPolicy struct {
	MaxStale   time.Duration
	CloseGrace time.Duration
	FutureSkew time.Duration
	Calendar   *Calendar
}

// DefaultFreshnessPolicy returns the A3 defaults for the supplied calendar.
func DefaultFreshnessPolicy(calendar *Calendar) FreshnessPolicy {
	if calendar == nil {
		calendar = Default()
	}
	return FreshnessPolicy{
		MaxStale:   DefaultMaxStale,
		CloseGrace: DefaultCloseGrace,
		FutureSkew: DefaultFutureSkew,
		Calendar:   calendar,
	}
}

func (p FreshnessPolicy) normalized() FreshnessPolicy {
	if p.MaxStale <= 0 {
		p.MaxStale = DefaultMaxStale
	}
	if p.CloseGrace == 0 {
		p.CloseGrace = DefaultCloseGrace
	}
	if p.FutureSkew <= 0 {
		p.FutureSkew = DefaultFutureSkew
	}
	if p.Calendar == nil {
		p.Calendar = Default()
	}
	return p
}

// ClassifySource applies the legacy/unknown-source rule before classifying a
// quote timestamp. Prices with no provenance or legacy provenance are
// deliberately reported as unknown rather than fresh.
func (p FreshnessPolicy) ClassifySource(source string, asOf, now time.Time) Freshness {
	if source == "" || source == SourceLegacy {
		return FreshnessUnknown
	}
	return p.Classify(asOf, now)
}

// Classify returns fresh when the quote satisfies the calendar-aware policy
// for now, and stale otherwise. A zero timestamp is unknown.
func (p FreshnessPolicy) Classify(asOf, now time.Time) Freshness {
	if asOf.IsZero() || now.IsZero() {
		return FreshnessUnknown
	}

	p = p.normalized()
	calendar := p.Calendar

	// A quote more than FutureSkew ahead of the clock is not trustworthy.
	if asOf.After(now.Add(p.FutureSkew)) {
		return FreshnessStale
	}

	now = now.In(calendar.Location())
	asOf = asOf.UTC()
	age := now.Sub(asOf)
	if age < 0 {
		age = 0
	}

	if calendar.IsTradingDay(now) {
		open := calendar.SessionOpen(now)
		close := calendar.SessionClose(now)

		switch {
		case now.Before(open):
			return freshAfterReference(asOf, calendar.PreviousSessionClose(now), p.CloseGrace)
		case now.Before(close):
			if !asOf.Before(open) && age <= p.MaxStale {
				return FreshnessFresh
			}
			return FreshnessStale
		default:
			if !asOf.Before(close.Add(-p.CloseGrace)) {
				return FreshnessFresh
			}
			return FreshnessStale
		}
	}

	return freshAfterReference(asOf, calendar.PreviousSessionClose(now), p.CloseGrace)
}

func freshAfterReference(asOf, reference time.Time, grace time.Duration) Freshness {
	if reference.IsZero() {
		return FreshnessUnknown
	}
	if !asOf.Before(reference.Add(-grace)) {
		return FreshnessFresh
	}
	return FreshnessStale
}
